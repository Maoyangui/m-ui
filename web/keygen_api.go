package web

import (
	"errors"
	mrand "math/rand"
	"net"
	"net/http"
	"strconv"

	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database/model"
)

// handleKeygen 为线路表单生成密钥材料:
//
//	GET /api/keygen?type=reality   → {privateKey, publicKey}
//	GET /api/keygen?type=uuid      → {uuid}
//	GET /api/keygen?type=shortid   → {shortId}
//	GET /api/keygen?type=port      → {port}(新线路的默认端口)
//	GET /api/keygen?type=password[&len=16] → {password}
func (s *Server) handleKeygen(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("type") {
	case "reality":
		priv, pub, err := creds.RealityKeypair()
		if err != nil {
			badRequest(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"privateKey": priv, "publicKey": pub})
	case "uuid":
		writeJSON(w, http.StatusOK, map[string]string{"uuid": creds.UUID()})
	case "shortid":
		writeJSON(w, http.StatusOK, map[string]string{"shortId": creds.ShortID()})
	case "port":
		port, err := s.freePort()
		if err != nil {
			badRequest(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"port": port})
	case "password":
		n, _ := strconv.Atoi(r.URL.Query().Get("len"))
		if n < 6 || n > 64 {
			n = 16
		}
		writeJSON(w, http.StatusOK, map[string]string{"password": creds.Password(n)})
	default:
		badRequest(w, errUnknownKeyType)
	}
}

var errUnknownKeyType = &keyErr{"type 需为 reality/uuid/shortid/password"}

type keyErr struct{ s string }

func (e *keyErr) Error() string { return e.s }

// freePort 挑一个可用的 5 位端口给新线路做默认值:
// 避开已有线路与面板/订阅/代理面板,再真正 bind 一次确认系统里也没别的进程占着。
func (s *Server) freePort() (int, error) {
	used := map[int]bool{}
	var ports []int
	s.db.Model(&model.Line{}).Pluck("port", &ports)
	for _, p := range ports {
		used[p] = true
	}
	used[s.settingInt("webPort", 2053)] = true
	used[s.settingInt("subPort", 2056)] = true
	used[s.settingInt("resellerPort", 2054)] = true

	for i := 0; i < 300; i++ {
		p := 10000 + mrand.Intn(55536) // 10000-65535,五位
		if used[p] || !portBindable(p) {
			continue
		}
		return p, nil
	}
	return 0, errors.New("没找到空闲端口,请手动填写")
}

// portBindable TCP 与 UDP 都能监听才算空闲(hysteria2 / tuic 走 UDP)。
func portBindable(port int) bool {
	addr := ":" + strconv.Itoa(port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}
	pc.Close()
	return true
}
