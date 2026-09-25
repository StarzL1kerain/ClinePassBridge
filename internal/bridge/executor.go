package bridge

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

type upstreamStream struct {
	deadline   time.Time
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}
type readChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func (s *Service) prepare(r ExecutorRequest) (map[string]any, Credential, string, error) {
	c, e := s.selectedCredential(r)
	if e != nil {
		return nil, c, "", e
	}
	// OAuth 凭据进入续期窗口时先行续期，避免请求打到已过期的令牌上。
	c = s.renewIfNeeded(c, r.HostCallbackID)
	up, e := s.resolveModel(r.Model)
	if e != nil {
		return nil, c, "", e
	}
	j, e := decodeObject(r.Payload)
	if e != nil {
		return nil, c, "", fail(400, "请求 JSON 无效")
	}
	if len(list(j["messages"])) == 0 {
		return nil, c, "", fail(400, "messages 必须是非空数组")
	}
	j["model"] = up
	// 当前 Pass 规划器会静默忽略这些参数，因此不能保留“指定了 provider”的假象。
	// 注意 object() 只对对象生效，字符串形式的 provider 也必须一并拦截。
	if v, ok := j["provider"]; ok && v != nil {
		return nil, c, "", fail(400, "该 Cline Pass 接入不支持指定 provider")
	}
	if v, ok := j["providerOptions"]; ok && v != nil {
		if p := object(object(v)["gateway"]); len(p) > 0 {
			return nil, c, "", fail(400, "Cline 当前会忽略 providerOptions.gateway 的指定，请使用自动路由")
		}
	}
	return j, c, up, nil
}
func headers(c Credential) http.Header {
	// OAuth 凭据的令牌已带 workos: 前缀；缺少该前缀上游会返回 401。
	return http.Header{"Authorization": []string{"Bearer " + c.bearerToken()}, "Content-Type": []string{"application/json"}, "User-Agent": []string{"ClinePassBridge/" + Version}}
}
func (s *Service) request(r ExecutorRequest, c Credential, j map[string]any, stream bool) (upstreamStream, error) {
	j["stream"] = stream
	if stream {
		opts := object(j["stream_options"])
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		j["stream_options"] = opts
	} else {
		delete(j, "stream_options")
	}
	return s.openUpstream(map[string]any{"host_callback_id": r.HostCallbackID, "method": "POST", "url": s.config().BaseURL + "/chat/completions", "headers": headers(c), "body": jsonBytes(j)}, r.deadline)
}

func (s *Service) openUpstream(payload any, deadline time.Time) (upstreamStream, error) {
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	}
	type result struct {
		up  upstreamStream
		err error
	}
	results := make(chan result)
	abandoned := make(chan struct{})
	defer close(abandoned)
	if err := s.begin(); err != nil {
		return upstreamStream{}, err
	}
	go func() {
		defer s.active.Done()
		var up upstreamStream
		err := s.call("host.http.do_stream", payload, &up)
		select {
		case results <- result{up, err}:
		case <-abandoned:
			s.closeUpstream(up.StreamID)
		}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var out upstreamStream
	select {
	case result := <-results:
		out = result.up
		if result.err != nil {
			s.closeUpstream(out.StreamID)
			return out, fail(502, "上游传输失败："+safeError(result.err))
		}
	case <-timer.C:
		return out, fail(504, "Cline 上游请求超时")
	case <-s.stopCh:
		return out, fail(503, "ClinePassBridge 正在关闭")
	}
	out.deadline = deadline
	if out.StreamID == "" {
		return out, fail(502, "宿主未返回上游流")
	}
	s.mu.Lock()
	stopped := s.stopped
	if !stopped {
		s.streams[out.StreamID] = struct{}{}
	}
	s.mu.Unlock()
	if stopped {
		s.closeUpstream(out.StreamID)
		return out, fail(503, "ClinePassBridge 正在关闭")
	}
	return out, nil
}
func (s *Service) closeUpstream(id string) {
	if id != "" {
		_ = s.call("host.http.stream_close", map[string]any{"stream_id": id}, nil)
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
	}
}
func (s *Service) read(up upstreamStream, fn func([]byte) error) error {
	cfg := s.config()
	var timedOut bool
	var mu sync.Mutex
	deadline := up.deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	}
	timerDone := make(chan struct{})
	timer := time.AfterFunc(time.Until(deadline), func() { defer close(timerDone); mu.Lock(); timedOut = true; mu.Unlock(); s.closeUpstream(up.StreamID) })
	defer func() {
		if !timer.Stop() {
			<-timerDone
		}
	}()
	defer s.closeUpstream(up.StreamID)
	total := 0
	for {
		var chunk readChunk
		e := s.call("host.http.stream_read", map[string]any{"stream_id": up.StreamID}, &chunk)
		mu.Lock()
		timeout := timedOut
		mu.Unlock()
		if timeout {
			return fail(504, "Cline upstream request timed out")
		}
		if e != nil {
			return fail(502, "读取上游失败："+safeError(e))
		}
		if chunk.Error != "" {
			return fail(502, "上游流中断："+safeError(errors.New(chunk.Error)))
		}
		total += len(chunk.Payload)
		if total > cfg.MaxResponseBytes {
			return fail(502, "上游响应超过配置上限")
		}
		if len(chunk.Payload) > 0 {
			if e = fn(chunk.Payload); e != nil {
				return e
			}
		}
		if chunk.Done {
			return nil
		}
	}
}
func (s *Service) readJSON(up upstreamStream) ([]byte, error) {
	var b bytes.Buffer
	e := s.read(up, func(v []byte) error { b.Write(v); return nil })
	if e != nil {
		return nil, e
	}
	if up.StatusCode < 200 || up.StatusCode >= 300 {
		j, _ := decodeObject(b.Bytes())
		return nil, fail(up.StatusCode, errorMessage(j))
	}
	return b.Bytes(), nil
}
func (s *Service) newLog(r ExecutorRequest, c Credential, up string) LogEntry {
	return LogEntry{ID: id(), Time: time.Now().UTC(), Model: r.Model, UpstreamModel: up, Stream: r.Stream, Provider: "unknown", ProviderSource: "not_reported", Credential: c.Label, Attempts: []Attempt{}}
}
func (s *Service) execute(r ExecutorRequest) (any, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.active.Done()
	r.Stream = false
	r.deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	j, c, up, e := s.prepare(r)
	entry := s.newLog(r, c, up)
	start := time.Now()
	defer func() {
		entry.DurationMS = time.Since(start).Milliseconds()
		entry.Status = statusOf(e)
		entry.Error = safeError(e)
		s.appendLog(entry)
	}()
	if e != nil {
		return nil, e
	}
	cfg := s.config()
	wantStream := cfg.NonstreamMode == "stream-aggregate"
	var body []byte
	for n := 0; n < 2; n++ {
		attempt := Attempt{Provider: "unknown", ProviderSource: "not_reported", Mode: "nonstream"}
		if wantStream {
			attempt.Mode = "stream-aggregate"
		}
		t := time.Now()
		var us upstreamStream
		us, e = s.request(r, c, j, wantStream)
		if e == nil {
			if wantStream {
				var cp *completion
				cp, e = s.consumeSSE(us, r.Model, &entry, &attempt, start, nil)
				if e == nil {
					body, e = cp.result(r.Model)
				}
			} else {
				var raw []byte
				raw, e = s.readJSON(us)
				if e == nil {
					var o map[string]any
					o, e = unwrap(raw)
					if e == nil {
						observeMetadata(o, &entry, &attempt)
						o["model"] = r.Model
						body = jsonBytes(o)
					}
				}
			}
		}
		attempt.Status = statusOf(e)
		attempt.Error = safeError(e)
		attempt.DurationMS = time.Since(t).Milliseconds()
		entry.Attempts = append(entry.Attempts, attempt)
		if e == nil {
			break
		}
		if n == 0 && !wantStream && cfg.NonstreamMode == "native-fallback" && isEmptyError(e) {
			wantStream = true
			continue
		}
		break
	}
	if e != nil {
		return nil, e
	}
	return Response{Payload: body, Headers: http.Header{"Content-Type": []string{"application/json"}}}, nil
}
func (s *Service) consumeSSE(us upstreamStream, model string, entry *LogEntry, attempt *Attempt, start time.Time, emit func([]byte) error) (*completion, error) {
	if us.StatusCode < 200 || us.StatusCode >= 300 {
		_, e := s.readJSON(us)
		return nil, e
	}
	if !strings.Contains(strings.ToLower(us.Headers.Get("Content-Type")), "text/event-stream") {
		_, e := s.readJSON(us)
		if e != nil {
			return nil, e
		}
		return nil, fail(502, "Cline 对流式请求返回了非 SSE 内容")
	}
	cp := newCompletion()
	decoder := SSEDecoder{max: s.config().MaxResponseBytes}
	e := s.read(us, func(b []byte) error {
		return decoder.Feed(b, func(payload []byte, event string) error {
			if strings.TrimSpace(string(payload)) == "[DONE]" {
				cp.done = true
				if !cp.allFinished() {
					return fail(502, "上游流结束时缺少 finish_reason")
				}
				// CPA owns the downstream SSE envelope and terminal marker.
				return errStreamDone
			}
			j, err := decodeObject(payload)
			if err != nil {
				return err
			}
			if j["error"] != nil || event == "error" || j["success"] == false {
				return fail(502, errorMessage(j))
			}
			observeMetadata(j, entry, attempt)
			cp.observe(j)
			if entry.TTFTMS == 0 && contentStarted(j) {
				entry.TTFTMS = time.Since(start).Milliseconds()
			}
			if emit != nil {
				j["model"] = model
				return emit(jsonBytes(j))
			}
			return nil
		})
	})
	if errors.Is(e, errStreamDone) {
		e = nil
	}
	if e == nil && !cp.done {
		e = decoder.End()
		if e == nil {
			e = fail(502, "上游流在收到 [DONE] 之前就结束了")
		}
	}
	return cp, e
}
func (s *Service) executeStream(r ExecutorRequest) (any, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	r.Stream = true
	r.deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	j, c, up, e := s.prepare(r)
	entry := s.newLog(r, c, up)
	start := time.Now()
	failEarly := func(err error) (any, error) {
		s.active.Done()
		entry.Status = statusOf(err)
		entry.Error = safeError(err)
		entry.DurationMS = time.Since(start).Milliseconds()
		entry.Attempts = append(entry.Attempts, Attempt{Status: entry.Status, Error: entry.Error, Mode: "stream", Provider: "unknown", DurationMS: entry.DurationMS})
		s.appendLog(entry)
		return nil, err
	}
	if e != nil {
		return failEarly(e)
	}
	if r.StreamID == "" {
		return failEarly(fail(500, "宿主未提供输出流标识"))
	}
	us, e := s.request(r, c, j, true)
	if e != nil {
		return failEarly(e)
	}
	if us.StatusCode < 200 || us.StatusCode >= 300 {
		_, e = s.readJSON(us)
		return failEarly(e)
	}
	go func() {
		defer s.active.Done()
		attempt := Attempt{Mode: "stream", Provider: "unknown", ProviderSource: "not_reported"}
		var err error
		defer func() {
			if recover() != nil {
				err = fail(500, "ClinePassBridge 流处理失败")
			}
			attempt.Status = statusOf(err)
			attempt.Error = safeError(err)
			attempt.DurationMS = time.Since(start).Milliseconds()
			entry.Attempts = append(entry.Attempts, attempt)
			entry.Status = attempt.Status
			entry.Error = attempt.Error
			entry.DurationMS = attempt.DurationMS
			s.appendLog(entry)
			_ = s.call("host.stream.close", map[string]any{"stream_id": r.StreamID, "error": safeError(err)}, nil)
		}()
		_, err = s.consumeSSE(us, r.Model, &entry, &attempt, start, func(b []byte) error {
			// CPA v7.3.12 passes native Chat Completions through as raw JSON,
			// but its OpenAI-to-Claude translator requires SSE input. The host
			// rewrites Format/SourceFormat; request_path preserves the HTTP route.
			if str(r.Metadata["request_path"]) == "/v1/messages" {
				b = append(append([]byte("data: "), b...), []byte("\n\n")...)
			}
			if e := s.call("host.stream.emit", map[string]any{"stream_id": r.StreamID, "payload": b}, nil); e != nil {
				return fail(499, "客户端已断开")
			}
			return nil
		})
	}()
	return map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}, "Cache-Control": []string{"no-cache"}}}, nil
}
