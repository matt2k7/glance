// Live updates for the "server-stats" widget.
//
// The backend exposes a per-widget SSE stream at
// /api/widgets/{id}/live (see widget-server-stats.go) which pushes a
// fresh snapshot of every configured server roughly every
// live-update-interval seconds. Rather than re-rendering the whole
// widget (which would lose popover state, scroll position, etc.) we
// patch the handful of dynamic values - CPU/RAM/disk percentages,
// progress bar widths and the popover detail rows - directly in the
// existing DOM, giving the same "live" feel as GoDoxy's system monitor.
//
// Reachability, uptime and the platform name are intentionally left
// alone here: if a remote agent goes offline or comes back online the
// widget will reflect that on the next full page load.

function formatMegabytes(mb) {
    var value;
    var label;

    if (mb < 1000) {
        value = String(mb);
        label = "MB";
    } else if (mb < 1000000) {
        value = mb < 10000 ? (mb / 1000).toFixed(1) : String(Math.floor(mb / 1000));
        label = "GB";
    } else {
        value = (mb / 1000000).toFixed(1);
        label = "TB";
    }

    return value + ' <span class="color-base size-h5">' + label + "</span>";
}

function statSelector(stat) {
    return '[data-stat="' + stat + '"]';
}

function setText(scope, stat, value) {
    if (value === undefined) return;

    var element = scope.querySelector(statSelector(stat));
    if (element) element.textContent = value;
}

function setFormattedMB(scope, stat, mb) {
    if (mb === undefined) return;

    var element = scope.querySelector(statSelector(stat));
    if (element) element.innerHTML = formatMegabytes(mb);
}

function setBar(scope, stat, percent) {
    if (percent === undefined) return;

    var bar = scope.querySelector(statSelector(stat));
    if (!bar) return;

    bar.style.setProperty("--percent", percent);
    bar.classList.toggle("progress-value-notice", percent >= 85);
}

function updateMountpoints(serverElement, mountpoints) {
    if (!Array.isArray(mountpoints)) return;

    for (var i = 0; i < mountpoints.length; i++) {
        var mountpoint = mountpoints[i];
        if (!mountpoint || !mountpoint.path) continue;

        var selector = '[data-mountpoint-path="' + CSS.escape(mountpoint.path) + '"]';
        var item = serverElement.querySelector(selector);
        if (!item) continue;

        setFormattedMB(item, "mountpoint-used", mountpoint.used_mb);
        setFormattedMB(item, "mountpoint-total", mountpoint.total_mb);
    }
}

function updateServer(serverElement, stat) {
    if (!stat || !stat.reachable || !stat.info) return;

    var info = stat.info;

    if (info.cpu && info.cpu.load_is_available) {
        setText(serverElement, "cpu-percent", info.cpu.load1_percent);
        setText(serverElement, "cpu-load1-popover", info.cpu.load1_percent);
        setText(serverElement, "cpu-load15-popover", info.cpu.load15_percent);
        setBar(serverElement, "cpu-load1-bar", info.cpu.load1_percent);
        setBar(serverElement, "cpu-load15-bar", info.cpu.load15_percent);
    }

    if (info.cpu && info.cpu.temperature_is_available) {
        setText(serverElement, "cpu-temp-popover", info.cpu.temperature_c);
    }

    if (info.memory && info.memory.memory_is_available) {
        setText(serverElement, "mem-percent", info.memory.used_percent);
        setBar(serverElement, "mem-bar", info.memory.used_percent);
        setFormattedMB(serverElement, "mem-used", info.memory.used_mb);
        setFormattedMB(serverElement, "mem-total", info.memory.total_mb);

        if (info.memory.swap_is_available) {
            setBar(serverElement, "swap-bar", info.memory.swap_used_percent);
            setFormattedMB(serverElement, "swap-used", info.memory.swap_used_mb);
            setFormattedMB(serverElement, "swap-total", info.memory.swap_total_mb);
        }
    }

    if (Array.isArray(info.mountpoints) && info.mountpoints.length > 0) {
        setText(serverElement, "disk-percent", info.mountpoints[0].used_percent);
        setBar(serverElement, "disk-bar-0", info.mountpoints[0].used_percent);

        if (info.mountpoints.length > 1) {
            setBar(serverElement, "disk-bar-1", info.mountpoints[1].used_percent);
        }

        updateMountpoints(serverElement, info.mountpoints);
    }
}

export default function (widgetElement) {
    var widgetId = widgetElement.dataset.widgetId;
    if (!widgetId) return;

    var serverElements = Array.from(
        widgetElement.querySelectorAll(":scope > .widget-content > .server")
    );
    if (serverElements.length === 0) return;

    var streamUrl = pageData.baseURL + "/api/widgets/" + widgetId + "/live";
    var source = new EventSource(streamUrl);

    source.onmessage = function (event) {
        var stats;

        try {
            stats = JSON.parse(event.data);
        } catch (err) {
            return;
        }

        if (!Array.isArray(stats)) return;

        for (var i = 0; i < stats.length; i++) {
            var serverElement = serverElements[i];
            if (serverElement) updateServer(serverElement, stats[i]);
        }
    };
}
