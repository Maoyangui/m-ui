package ha

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
)

// 命令行(m-ui ha …,要 root)和运行中的服务之间走库:命令行把请求写进本机设置 haCmd,服务每 5 秒看一眼,
// 做完把结果写进 haCmdResult;命令行等结果打印出来。服务没在跑时,status 直接读库给个大概。

// Cmd 命令行交给服务的一个请求。
type Cmd struct {
	Id     string `json:"id"`
	Op     string `json:"op"` // status | takeover | set-master | pause | resume
	Target string `json:"target,omitempty"`
	Force  bool   `json:"force,omitempty"`
	Min    int    `json:"min,omitempty"`
	At     int64  `json:"at"`
}

// CmdResult 服务做完的结果。
type CmdResult struct {
	Id   string          `json:"id"`
	OK   bool            `json:"ok"`
	Err  string          `json:"err,omitempty"`
	Text string          `json:"text,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	At   int64           `json:"at"`
}

// cmdTick 服务这边:有命令行的请求就做。
func (s *Service) cmdTick() {
	var c Cmd
	if !s.getJSON(KeyCmd, &c) || c.Id == "" {
		return
	}
	var done CmdResult
	if s.getJSON(KeyCmdResult, &done) && done.Id == c.Id {
		return
	}
	if s.now().Unix()-c.At > 300 { // 命令行早就不等了
		_ = s.set(map[string]string{KeyCmd: ""})
		return
	}
	res := CmdResult{Id: c.Id, At: s.now().Unix()}
	err := s.runCmd(c, &res)
	if err != nil {
		res.Err = err.Error()
	} else {
		res.OK = true
	}
	_ = s.set(map[string]string{KeyCmdResult: mustJSON(res), KeyCmd: ""})
	logger.Info("容灾:命令行 m-ui ha ", c.Op, " → ", map[bool]string{true: "完成", false: "失败"}[res.OK])
}

func (s *Service) runCmd(c Cmd, res *CmdResult) error {
	switch c.Op {
	case "status":
		b, _ := json.Marshal(s.View(true))
		res.Data = b
		return nil
	case "takeover":
		if !s.IsNode() {
			if !c.Force {
				return errors.New("本机已经是主机。主机和别的主机僵持时,要以更高的纪元把副机收回来,加 --force")
			}
			e, err := s.reclaim(0, true)
			if err != nil {
				return err
			}
			res.Text = fmt.Sprintf("本机以纪元 %d 继续当主机,已给认本机的副机发交接标记", e)
			return nil
		}
		if err := s.Takeover(TakeoverOpts{Force: c.Force, By: "cli"}); err != nil {
			return err
		}
		res.Text = "本机正在接任为主机,马上重启"
		return nil
	case "set-master":
		n, err := s.findNode(c.Target)
		if err != nil {
			return err
		}
		st, err := s.SetMaster(n.Id, "cli")
		if err != nil {
			return err
		}
		b, _ := json.Marshal(st)
		res.Data = b
		res.Text = "已开始把主机换到 " + n.Name
		return nil
	case "pause":
		if err := s.Pause(c.Min); err != nil {
			return err
		}
		res.Text = fmt.Sprintf("已暂停接任 %d 分钟", c.Min)
		return nil
	case "resume":
		if err := s.Pause(0); err != nil {
			return err
		}
		res.Text = "已恢复接任"
		return nil
	}
	return fmt.Errorf("不认识的操作 %q", c.Op)
}

// findNode 按名字或 id 找服务器。
func (s *Service) findNode(target string) (model.Node, error) {
	var n model.Node
	target = strings.TrimSpace(target)
	if target == "" {
		return n, errors.New("要指定服务器:名字或 id")
	}
	q := s.d.DB.Where("name = ?", target)
	if id, err := strconv.ParseUint(target, 10, 32); err == nil {
		q = s.d.DB.Where("id = ? OR name = ?", id, target)
	}
	if err := q.First(&n).Error; err != nil {
		return n, fmt.Errorf("没有这台服务器:%s", target)
	}
	return n, nil
}

// SubmitCmd 命令行这边:把请求写进库,等服务做完(最多 wait),返回结果。服务没在跑时返回错误。
func SubmitCmd(db *gorm.DB, c Cmd, wait time.Duration) (CmdResult, error) {
	c.Id, c.At = randID(), time.Now().Unix()
	if err := db.Transaction(func(tx *gorm.DB) error { return upsert(tx, map[string]string{KeyCmd: mustJSON(c)}) }); err != nil {
		return CmdResult{}, err
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		var raw string
		db.Raw("SELECT value FROM settings WHERE key = ?", KeyCmdResult).Scan(&raw)
		var r CmdResult
		if json.Unmarshal([]byte(raw), &r) == nil && r.Id == c.Id {
			return r, nil
		}
	}
	// 没人接:撤回请求,免得服务之后起来又去做
	db.Exec("UPDATE settings SET value = '' WHERE key = ? AND value LIKE ?", KeyCmd, "%"+c.Id+"%")
	return CmdResult{}, errors.New("m-ui 服务没有回应:它在运行吗?(systemctl status m-ui)")
}
