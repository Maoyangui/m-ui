package web

import (
	"net/http"
	"strconv"

	"github.com/Maoyangui/m-ui/creds"
)

// handleKeygen 为线路表单生成密钥材料:
//
//	GET /api/keygen?type=reality   → {privateKey, publicKey}
//	GET /api/keygen?type=uuid      → {uuid}
//	GET /api/keygen?type=shortid   → {shortId}
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
