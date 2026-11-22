package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/safe"
)

// ---- 外部 API 的调用日志(用户 11-22:「密钥这里 对应的密钥行里增加一个日志 点击可显示查看调用日志(白话版看得懂的
// 鼠标悬浮显示具体执行指令 代理面板也一样) 可设置不开启日志 / 自动删除 / 一键删除」) ----
// 每把密钥一份:什么时候、从哪个 IP、调了什么、结果怎样。界面按 Action / Target 拼成大白话,悬停看原始请求(方法、路径、参数、请求体)。
// 只在本机记,不进主从同步;请求体里订阅地址、令牌之类的值打码(apiLogBody),响应不记。自动删除见 jobs 的 pruneAPIKeyLogs。

const (
	apiLogBodyMax  = 600 // 记下的请求体最多这么多字(悬停时看个大概够了)
	apiLogQueryMax = 300
	apiLogErrMax   = 200
)

// apiLogSecretKeys 请求体里这些键的值不进日志:/users/lookup 的 sub 是用户的订阅地址(就是凭据,特意放在请求体里、不进网址和访问日志),
// 别的是保险(现在的接口不收这些字段)。
var apiLogSecretKeys = map[string]bool{"sub": true, "token": true, "subtoken": true, "password": true, "pass": true, "secret": true,
	"key": true, "url": true, "link": true, "uuid": true, "credentials": true}

// apiLogRec 记下处理完的状态码;失败时留一点正文好取出原因({"error": "…"})。
type apiLogRec struct {
	http.ResponseWriter
	status  int
	errBody []byte
}

func (w *apiLogRec) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *apiLogRec) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && len(w.errBody) < 4096 {
		n := len(b)
		if n > 4096-len(w.errBody) {
			n = 4096 - len(w.errBody)
		}
		w.errBody = append(w.errBody, b[:n]...)
	}
	return w.ResponseWriter.Write(b)
}

// errReader 请求体读到一半出错(超过大小上限):后面的处理照旧读到这个错误
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// apiLogStart 这次调用要记日志(有对应的密钥、它没关日志)就把请求体读出来留一份、再原样塞回去,
// 返回包了一层的 ResponseWriter 和收尾函数(处理完调用,写一条日志);不记的原样返回、收尾什么也不做。
func (s *Server) apiLogStart(w http.ResponseWriter, r *http.Request, sc apiScope) (http.ResponseWriter, func()) {
	if sc.keyId == 0 || sc.logOff || s.db == nil {
		return w, func() {}
	}
	start := time.Now()
	var body []byte
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		b, err := io.ReadAll(r.Body)
		body = b
		if err != nil {
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), errReader{err}))
		} else {
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
	}
	rec := &apiLogRec{ResponseWriter: w}
	ip := s.sourceIP(r)
	return rec, func() {
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		path, parts := apiLogPath(r.URL.Path)
		row := model.ApiKeyLog{KeyId: sc.keyId, At: start.Unix(), IP: ip, Method: r.Method, Path: path,
			Query: clipRunes(r.URL.RawQuery, apiLogQueryMax), Body: apiLogBody(body), Status: status,
			Ms: int(time.Since(start) / time.Millisecond)}
		row.Action, row.Target = apiLogAction(r.Method, parts, body)
		if status >= 400 {
			row.Err = apiLogErr(rec.errBody)
		}
		s.apiLogPush(row)
	}
}

// apiLogPush 日志排队写:一个协程按调用结束的先后依次写,列表按 id 排就是先后(各起协程写的话,先结束的可能后落库);
// 不让写日志拖慢调用的响应:队列满了(库卡住了)就丢掉这一条。
func (s *Server) apiLogPush(row model.ApiKeyLog) {
	s.apiLogOnce.Do(func() {
		s.apiLogCh = make(chan model.ApiKeyLog, 1024)
		db, ch := s.db, s.apiLogCh
		safe.Go("web.apiLogWriter", func() {
			for r := range ch {
				db.Create(&r)
			}
		})
	})
	select {
	case s.apiLogCh <- row:
	default:
	}
}

// apiLogPath 记下的路径从 /api/v1/ 起(前面是面板的路径,本身就是秘密,不进日志),和接口文档里的写法一样;parts 是 /v1/ 之后的各段。
func apiLogPath(full string) (string, []string) {
	full = strings.TrimSuffix(full, "/")
	idx := strings.LastIndex(full, "/v1/")
	if idx < 0 {
		return "/api/v1/", nil
	}
	rest := full[idx+4:]
	return "/api/v1/" + rest, strings.Split(rest, "/")
}

// apiLogBody 请求体:JSON 的话把敏感键的值换成 ***(任意层级),再压成一行、截到 apiLogBodyMax 字;不是 JSON 的只记多少字节。
func apiLogBody(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return "(" + strconv.Itoa(len(b)) + " bytes)"
	}
	out, _ := json.Marshal(maskAPILog(v))
	return clipRunes(string(out), apiLogBodyMax)
}

func maskAPILog(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, e := range x {
			if apiLogSecretKeys[strings.ToLower(k)] {
				x[k] = "***"
			} else {
				x[k] = maskAPILog(e)
			}
		}
	case []interface{}:
		for i, e := range x {
			x[i] = maskAPILog(e)
		}
	}
	return v
}

// apiLogAction 这次调用做了什么(界面按它拼大白话)、对谁;认不出的记 unknown,界面就只显示原始请求。
func apiLogAction(method string, parts []string, body []byte) (action, target string) {
	if len(parts) == 0 {
		return "unknown", ""
	}
	field := func(k string) string {
		var m map[string]interface{}
		if json.Unmarshal(body, &m) != nil {
			return ""
		}
		switch v := m[k].(type) {
		case string:
			return clipRunes(v, 64)
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	switch parts[0] {
	case "ping", "overview", "servers", "lines", "onlines", "plans":
		if len(parts) == 1 {
			return parts[0], ""
		}
	case "users":
		switch {
		case len(parts) == 1 && method == http.MethodGet:
			return "users.list", ""
		case len(parts) == 1 && method == http.MethodPost:
			return "user.create", field("name")
		case len(parts) == 2 && parts[1] == "lookup" && method == http.MethodPost:
			return "user.lookup", ""
		case len(parts) == 2 && parts[1] == "batch" && method == http.MethodPost:
			return "users.batch", field("count")
		case len(parts) == 2:
			switch method {
			case http.MethodGet:
				return "user.get", parts[1]
			case http.MethodPatch, http.MethodPut:
				return "user.update", parts[1]
			case http.MethodDelete:
				return "user.delete", parts[1]
			}
		case len(parts) == 3:
			switch parts[2] {
			case "traffic", "sub", "enable", "disable", "reset", "kick", "rotate", "change":
				return "user." + parts[2], parts[1]
			case "plan-ops":
				return "user.planOps", parts[1]
			case "plan", "renew":
				return "user.renew", parts[1]
			}
		}
	}
	return "unknown", ""
}

// apiLogErr 失败时的原因:返回的 {"error": "…"};取不到就空着(界面显示状态码)。
func apiLogErr(b []byte) string {
	var m struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return clipRunes(m.Error, apiLogErrMax)
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// handleKeyLogs …/api/keys/{id}/logs:GET 看日志(新的在前;?before=上一页最后一条的 id、?limit= 默认 50 最多 200),
// 一并给这把密钥的日志设置;PUT {off, days} 改设置(不记 / 留几天);DELETE 一键清空。
func (s *Server) handleKeyLogs(w http.ResponseWriter, r *http.Request, k model.ApiKey, audit func(action string, obj interface{})) {
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 200 {
			limit = 50
		}
		q := s.db.Where("key_id = ?", k.Id)
		if before, _ := strconv.ParseUint(r.URL.Query().Get("before"), 10, 64); before > 0 {
			q = q.Where("id < ?", before)
		}
		var rows []model.ApiKeyLog
		q.Order("id desc").Limit(limit + 1).Find(&rows)
		more := len(rows) > limit
		if more {
			rows = rows[:limit]
		}
		var total int64
		s.db.Model(&model.ApiKeyLog{}).Where("key_id = ?", k.Id).Count(&total)
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": rows, "more": more, "total": total,
			"logOff": k.LogOff, "logDays": model.APILogDays(k.LogDays), "dayChoices": model.APILogDayChoices})
	case http.MethodPut:
		var p struct {
			Off  *bool `json:"off"`
			Days *int  `json:"days"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&p); err != nil {
			badRequest(w, errors.New("请求格式不对"))
			return
		}
		upd := map[string]interface{}{}
		if p.Off != nil {
			upd["log_off"] = *p.Off
		}
		if p.Days != nil {
			if model.APILogDays(*p.Days) != *p.Days {
				badRequest(w, errors.New("调用日志只能留 1、7、30 或 90 天"))
				return
			}
			upd["log_days"] = *p.Days
		}
		if len(upd) == 0 {
			badRequest(w, errors.New("请求格式不对"))
			return
		}
		if err := s.db.Model(&model.ApiKey{}).Where("id = ?", k.Id).Updates(upd).Error; err != nil {
			badRequest(w, err)
			return
		}
		upd["id"], upd["name"] = k.Id, k.Name
		audit("api-key:log", upd)
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	case http.MethodDelete:
		res := s.db.Where("key_id = ?", k.Id).Delete(&model.ApiKeyLog{})
		if res.Error != nil {
			badRequest(w, res.Error)
			return
		}
		audit("api-key:log-clear", map[string]interface{}{"id": k.Id, "name": k.Name, "n": res.RowsAffected})
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "deleted": res.RowsAffected})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}
