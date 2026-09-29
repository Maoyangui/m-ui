package web

import (
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
)

// 登录接口不用登录就能调:超大请求体要在读完之前就被拒,而不是整个缓冲进内存。
func TestLoginRejectsHugeBody(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db, sessions: map[string]session{}, loginFails: map[string][]int64{}}
	for name, h := range map[string]func(w *httptest.ResponseRecorder, body io.Reader){
		"主面板": func(w *httptest.ResponseRecorder, body io.Reader) {
			s.handleLogin(w, httptest.NewRequest("POST", "http://x/app/api/login", body))
		},
		"代理面板": func(w *httptest.ResponseRecorder, body io.Reader) {
			s.handleResellerLogin(w, httptest.NewRequest("POST", "http://x/dl/api/login", body))
		},
	} {
		body := &countingReader{r: io.MultiReader(strings.NewReader(`{"username":"`), strings.NewReader(strings.Repeat("a", 64<<20)))}
		w := httptest.NewRecorder()
		h(w, body)
		if w.Code != 400 {
			t.Fatalf("%s:超大请求体应被拒,得 %d", name, w.Code)
		}
		if body.n > 64<<10 {
			t.Fatalf("%s:应在读到上限时就停,实际读了 %d 字节", name, body.n)
		}
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
