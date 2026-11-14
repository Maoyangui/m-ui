package web

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"gorm.io/gorm"
)

func fwRaw(fs ...model.LineForward) json.RawMessage { b, _ := json.Marshal(fs); return b }

// 中转入口保存前的校验:转给的服务器(自动 / 必须在部署范围)、中转服务器(存在、不是出口自己)、端口在中转服务器上
// 不撞(面板 / 订阅端口、那台的线路、别的中转入口、UDP 的端口跳跃范围、这条线路自己)、外部中转的地址、条数和名字长度;
// 反过来线路改端口 / 改部署范围时也避开已有的中转入口;删服务器时引用它的中转入口一起去掉。
func TestLineForwardsValidation(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	sp := json.RawMessage(`[{"label":"面板","port":2053},{"label":"订阅","port":2056}]`)
	master := model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1}
	gz := model.Node{Name: "广州中转", ApiUrl: "http://gz", Enabled: true, Sort: 2, ServicePorts: sp}
	jp := model.Node{Name: "东京", ApiUrl: "http://jp", Enabled: true, Sort: 3, ServicePorts: sp}
	osk := model.Node{Name: "大阪", ApiUrl: "http://os", Enabled: true, Sort: 4, ServicePorts: sp}
	for _, n := range []*model.Node{&master, &gz, &jp, &osk} {
		db.Create(n)
	}
	ids := func(v ...uint) json.RawMessage { b, _ := json.Marshal(v); return b }
	mustCreate := func(l model.Line) model.Line {
		t.Helper()
		if err := s.validateLine(&l); err != nil {
			t.Fatalf("%s 应通过: %v", l.Name, err)
		}
		if err := db.Create(&l).Error; err != nil {
			t.Fatal(err)
		}
		return l
	}
	mustCreate(model.Line{Name: "gz-line", Protocol: "trojan", Port: 8443, Enabled: true, NodeIds: ids(gz.Id)})
	mustCreate(model.Line{Name: "gz-hop", Protocol: "hysteria2", Port: 40443, Enabled: true, NodeIds: ids(gz.Id), Options: json.RawMessage(`{"port_hopping":"50000-50100"}`)})
	mustCreate(model.Line{Name: "other-fw", Protocol: "vless", Port: 9443, Enabled: true, NodeIds: ids(jp.Id), Forwards: fwRaw(model.LineForward{Via: gz.Id, Port: 31000, To: jp.Id})})

	line := func(proto string, nodes json.RawMessage, fs ...model.LineForward) model.Line {
		return model.Line{Name: "hy2", Protocol: proto, Port: 443, Enabled: true, NodeIds: nodes, Forwards: fwRaw(fs...)}
	}
	two := ids(jp.Id, osk.Id)
	expectErr := func(what string, l model.Line, want string) {
		t.Helper()
		err := s.validateLine(&l)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s:应报含 %q 的错,实际 %v", what, want, err)
		}
	}

	// 正常:经广州转给东京;名字去空白、外部字段清掉
	ok := line("hysteria2", two, model.LineForward{Name: "  广州  ", Via: gz.Id, Port: 30001, To: jp.Id, Host: "ignored"})
	if err := s.validateLine(&ok); err != nil {
		t.Fatalf("正常的中转入口应通过: %v", err)
	}
	if fs := model.ParseForwards(ok.Forwards); len(fs) != 1 || fs[0].Name != "广州" || fs[0].Host != "" {
		t.Fatalf("应规整:%s", ok.Forwards)
	}
	// 只部署一台时「转给」自动填上
	single := line("hysteria2", ids(jp.Id), model.LineForward{Via: gz.Id, Port: 30001})
	if err := s.validateLine(&single); err != nil || model.ParseForwards(single.Forwards)[0].To != jp.Id {
		t.Fatalf("只部署在东京时自动转给东京: %v %s", err, single.Forwards)
	}
	expectErr("两台时没选转给哪台", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 30001}), "没选转给哪台")
	expectErr("转给不在部署范围的", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 30001, To: master.Id}), "没部署在那台")
	expectErr("中转和出口同一台", line("hysteria2", two, model.LineForward{Via: jp.Id, Port: 30001, To: jp.Id}), "同一台")
	expectErr("中转服务器不在了", line("hysteria2", two, model.LineForward{Via: 99, Port: 30001, To: jp.Id}), "中转服务器已经不在了")
	expectErr("转给的服务器不在了", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 30001, To: 99}), "转给的服务器已经不在了")
	expectErr("撞中转服务器的面板端口", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 2053, To: jp.Id}), "面板")
	expectErr("撞那台的线路", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 8443, To: jp.Id}), "gz-line")
	expectErr("撞别的中转入口", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 31000, To: jp.Id}), "other-fw")
	expectErr("UDP 落进端口跳跃", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 50050, To: jp.Id}), "端口跳跃")
	if l := line("trojan", two, model.LineForward{Via: gz.Id, Port: 50050, To: jp.Id}); s.validateLine(&l) != nil {
		t.Fatal("只转 TCP 的不受 UDP 端口跳跃范围影响")
	}
	expectErr("同一条里重复", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 30001, To: jp.Id}, model.LineForward{Via: gz.Id, Port: 30001, To: osk.Id}), "同一个端口")
	expectErr("这条线路自己也在中转服务器上", line("hysteria2", ids(jp.Id, gz.Id), model.LineForward{Via: gz.Id, Port: 443, To: jp.Id}), "这条线路自己")
	// 外部中转
	ext := line("hysteria2", two, model.LineForward{Host: " [2001:db8::9] ", Port: 31000, To: osk.Id, Target: "x"})
	if err := s.validateLine(&ext); err != nil {
		t.Fatalf("外部中转(端口在别人机器上,不查撞端口)应通过: %v", err)
	}
	if f := model.ParseForwards(ext.Forwards)[0]; f.Host != "2001:db8::9" || f.Target != "" {
		t.Fatalf("外部中转地址去方括号、不留转到地址: %+v", f)
	}
	expectErr("外部中转带协议头", line("hysteria2", two, model.LineForward{Host: "http://hk.relay", Port: 1, To: jp.Id}), "只填 IP 或域名")
	expectErr("外部中转没填地址", line("hysteria2", two, model.LineForward{Port: 1, To: jp.Id}), "没填中转地址")
	expectErr("转到地址带端口", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 30001, To: jp.Id, Target: "10.0.0.2:443"}), "转到地址")
	expectErr("端口越界", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 70000, To: jp.Id}), "1-65535")
	expectErr("名字太长", line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 30001, To: jp.Id, Name: strings.Repeat("长", 41)}), "太长")
	many := make([]model.LineForward, model.MaxForwards+1)
	for i := range many {
		many[i] = model.LineForward{Host: fmt.Sprintf("r%d.example", i), Port: 1000 + i, To: jp.Id}
	}
	expectErr("太多", line("hysteria2", two, many...), "最多")

	// 反过来:部署在广州的新线路不能用别的线路的中转入口占着的端口
	expectErr("线路撞中转入口", model.Line{Name: "new-gz", Protocol: "trojan", Port: 31000, Enabled: true, NodeIds: ids(gz.Id)}, "中转入口")
	if l := (model.Line{Name: "new-jp", Protocol: "trojan", Port: 31000, Enabled: true, NodeIds: ids(jp.Id)}); s.validateLine(&l) != nil {
		t.Fatal("别的服务器上同端口不相干")
	}

	// 批量改部署范围:中转入口转给的那台被撤掉要拦下
	saved := mustCreate(line("hysteria2", two, model.LineForward{Via: gz.Id, Port: 30001, To: jp.Id}))
	saved.NodeIds = ids(osk.Id)
	if err := s.checkBatchPorts([]model.Line{saved}); err == nil || !strings.Contains(err.Error(), "东京") {
		t.Fatalf("批量改范围撤掉中转的出口应拦下: %v", err)
	}
	saved.NodeIds = ids(jp.Id, gz.Id)
	saved.Port = 30001
	if err := s.checkBatchPorts([]model.Line{saved}); err == nil || !strings.Contains(err.Error(), "30001") {
		t.Fatalf("批量改范围后线路落到中转服务器、和自己的中转入口撞端口应拦下: %v", err)
	}

	// 删服务器:经它中转、转给它的中转入口一起去掉
	var pruned []string
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		pruned, err = pruneForwardsTx(tx, gz.Id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 2 {
		t.Fatalf("other-fw 和 hy2 两条都该去掉经广州的中转: %v", pruned)
	}
	var after []model.Line
	db.Find(&after)
	for _, l := range after {
		if len(model.ParseForwards(l.Forwards)) != 0 {
			t.Fatalf("%s 还留着中转入口: %s", l.Name, l.Forwards)
		}
	}
}
