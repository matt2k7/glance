package glance

import (
	"context"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/glanceapp/glance/pkg/sysinfo"
)

var serverStatsWidgetTemplate = mustParseTemplate("server-stats.html", "widget-base.html")

// liveUpdateMinInterval is the lowest allowed interval between live stat
// collections. It also acts as the cache duration for collectLiveInfo,
// preventing multiple simultaneously connected clients (eg. several
// browser tabs) from each triggering their own gopsutil/agent calls.
const liveUpdateMinInterval = 1 * time.Second

type serverStatsWidget struct {
	widgetBase         `yaml:",inline"`
	Servers            []serverStatsRequest `yaml:"servers"`
	LiveUpdateInterval durationField        `yaml:"live-update-interval"`

	liveMu   sync.Mutex       `yaml:"-"`
	liveAt   time.Time        `yaml:"-"`
	liveData []serverLiveInfo `yaml:"-"`
}

func (widget *serverStatsWidget) initialize() error {
	widget.withTitle("Server Stats").withCacheDuration(15 * time.Second)
	widget.widgetBase.WIP = true

	if len(widget.Servers) == 0 {
		widget.Servers = []serverStatsRequest{{Type: "local"}}
	}

	for i := range widget.Servers {
		widget.Servers[i].URL = strings.TrimRight(widget.Servers[i].URL, "/")

		if widget.Servers[i].Timeout == 0 {
			widget.Servers[i].Timeout = durationField(3 * time.Second)
		}
	}

	if widget.LiveUpdateInterval < durationField(liveUpdateMinInterval) {
		widget.LiveUpdateInterval = durationField(2 * time.Second)
	}

	return nil
}

func (widget *serverStatsWidget) update(context.Context) {
	// Refactor later, most of it may change depending on feedback
	var wg sync.WaitGroup

	for i := range widget.Servers {
		serv := &widget.Servers[i]

		if serv.Type == "local" {
			info, errs := sysinfo.Collect(serv.SystemInfoRequest)

			if len(errs) > 0 {
				for i := range errs {
					slog.Warn("Getting system info: " + errs[i].Error())
				}
			}

			serv.IsReachable = true
			serv.Info = info
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				info, err := fetchRemoteServerInfo(serv)
				if err != nil {
					slog.Warn("Getting remote system info: " + err.Error())
					serv.IsReachable = false
					serv.Info = &sysinfo.SystemInfo{
						Hostname: "Unnamed server #" + strconv.Itoa(i+1),
					}
				} else {
					serv.IsReachable = true
					serv.Info = info
				}
			}()
		}
	}

	wg.Wait()
	widget.withError(nil).scheduleNextUpdate()
}

// serverLiveInfo is the shape sent to the browser over the live-update
// SSE stream. It mirrors sysinfo.SystemInfo (which already has the json
// tags the frontend expects) plus a reachability flag for remote agents.
type serverLiveInfo struct {
	Reachable bool                `json:"reachable"`
	Info      *sysinfo.SystemInfo `json:"info,omitempty"`
}

// fetchServerInfo performs a single, blocking collection of system info
// for a server entry - either locally via gopsutil or remotely via a
// glance-agent instance.
func fetchServerInfo(serv *serverStatsRequest) (*sysinfo.SystemInfo, bool) {
	if serv.Type == "local" {
		info, errs := sysinfo.Collect(serv.SystemInfoRequest)

		for i := range errs {
			slog.Debug("Getting system info for live update: " + errs[i].Error())
		}

		return info, true
	}

	info, err := fetchRemoteServerInfo(serv)
	if err != nil {
		slog.Debug("Getting remote system info for live update: " + err.Error())
		return nil, false
	}

	return info, true
}

// collectLiveInfo gathers fresh stats for every configured server,
// caching the result for liveUpdateMinInterval so that several
// concurrently connected SSE clients share a single collection pass.
func (widget *serverStatsWidget) collectLiveInfo() []serverLiveInfo {
	widget.liveMu.Lock()
	defer widget.liveMu.Unlock()

	if time.Since(widget.liveAt) < liveUpdateMinInterval {
		return widget.liveData
	}

	data := make([]serverLiveInfo, len(widget.Servers))

	var wg sync.WaitGroup
	for i := range widget.Servers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			info, reachable := fetchServerInfo(&widget.Servers[i])
			data[i] = serverLiveInfo{Reachable: reachable, Info: info}
		}(i)
	}
	wg.Wait()

	widget.liveData = data
	widget.liveAt = time.Now()

	return data
}

// handleRequest serves GET /api/widgets/{id}/live as a Server-Sent
// Events stream, pushing fresh stats for every configured server roughly
// every LiveUpdateInterval. The frontend (server-stats.js) keeps the
// already-rendered widget markup in place and patches the dynamic values
// in-place as events arrive, giving the same "live" feel as GoDoxy's
// system monitor without re-rendering the whole widget.
func (widget *serverStatsWidget) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("path") != "live" {
		http.NotFound(w, r)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	// Some reverse proxies (eg. nginx) buffer responses by default which
	// would defeat the purpose of an SSE stream.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	interval := time.Duration(widget.LiveUpdateInterval)
	if interval < liveUpdateMinInterval {
		interval = liveUpdateMinInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	writeUpdate := func() bool {
		payload, err := json.Marshal(widget.collectLiveInfo())
		if err != nil {
			slog.Error("Failed to marshal live server stats", "error", err)
			return true
		}

		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		if _, err := w.Write(payload); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}

		flusher.Flush()
		return true
	}

	// Send an initial snapshot immediately so the UI doesn't wait a full
	// interval before showing live data.
	if !writeUpdate() {
		return
	}

	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !writeUpdate() {
				return
			}
		}
	}
}

func (widget *serverStatsWidget) Render() template.HTML {
	return widget.renderTemplate(widget, serverStatsWidgetTemplate)
}

type serverStatsRequest struct {
	*sysinfo.SystemInfoRequest `yaml:",inline"`
	Info                       *sysinfo.SystemInfo `yaml:"-"`
	IsReachable                bool                `yaml:"-"`
	StatusText                 string              `yaml:"-"`
	Name                       string              `yaml:"name"`
	HideSwap                   bool                `yaml:"hide-swap"`
	Type                       string              `yaml:"type"`
	URL                        string              `yaml:"url"`
	Token                      string              `yaml:"token"`
	Timeout                    durationField       `yaml:"timeout"`
	// Support for other agents
	// Provider                   string              `yaml:"provider"`
}

func fetchRemoteServerInfo(infoReq *serverStatsRequest) (*sysinfo.SystemInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(infoReq.Timeout))
	defer cancel()

	request, _ := http.NewRequestWithContext(ctx, "GET", infoReq.URL+"/api/sysinfo/all", nil)
	if infoReq.Token != "" {
		request.Header.Set("Authorization", "Bearer "+infoReq.Token)
	}

	info, err := decodeJsonFromRequest[*sysinfo.SystemInfo](defaultHTTPClient, request)
	if err != nil {
		return nil, err
	}

	return info, nil
}
