package web

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/render"

	"gorm.io/gorm"
)

// errPreviewRollback 预演事务的哨兵:改动写进去只为渲染出"改完以后"的整份配置,渲染完一律回滚。
var errPreviewRollback = errors.New("预演完毕,回滚")

// errConfigMoved 正式写入时库已经被别人改过、渲染出来的配置和刚校验过的那份不一样:回滚,按新的这份再校验。
var errConfigMoved = errors.New("配置在校验期间有变化")

// commitRounds 正式写入最多追几轮"别人刚改过库";追不上就在事务里校验(兜底,坏配置照样不落库)。
const commitRounds = 3

// commitValidated 把一笔改动写进库,但只在改完以后的整份配置通过 sing-box 干跑之后才提交。
//
// 以前是在写事务里干跑:干跑要排队(core 的干跑锁同一时间只给一个人 —— 别的保存、数据面重载,
// 0.7.16 里还有测速用的临时实例,测一条不通的上游就整段占着它 5~10 秒),排队期间这笔事务一直占着 SQLite 的写锁,
// 主副机同步的并账、统计落库、审计、改用户全卡在后面,等满 busy_timeout(10 秒)就报 database is locked。
// 测试集群上 0.7.16 一边对连不上的上游测速、一边保存线路,能稳定卡出约 20 秒;生产上那次保存要转 20 秒
// 是不是这个原因没有证实(当时的日志里主副机同步没断过,和"写锁被占着"对不上),这里只是把这条路堵上。
//
// 现在分三步,都在同一条数据库连接上:
// ① 短事务里写入改动、渲染整份配置,随即回滚(只为拿到改完以后的配置),同时记下库的版本(PRAGMA data_version);
// ② 不持任何数据库锁做干跑,排队也只慢这一笔保存;
// ③ 再开短事务重新写入。库的版本没变(这期间没有别的连接提交过),写进去的就是刚校验过的那份,直接提交;
// 变了就重新渲染,和校验过的一字不差也提交,不一样就按新的这份再校验一轮。commitRounds 轮都追不上,最后一轮在事务里校验。
//
// apply 会被调用不止一次,必须可重入:每次都从同样的输入写起(新建的记录先把 id 清零)。
// apply 自己的错误原样返回;渲染或干跑失败返回带"已取消保存"的错误,库里什么都不留。
func (s *Server) commitValidated(apply func(tx *gorm.DB) error) error {
	var cert render.NodeCert
	if s.run != nil { // 事务里不能再走连接池,先取好;测试里可能没有数据面
		cert = s.run.NodeCert()
	}
	return s.db.Connection(func(conn *gorm.DB) error {
		var want []byte
		var ver int64
		err := conn.Transaction(func(tx *gorm.DB) error {
			var err error
			if ver, err = dataVersion(tx); err != nil {
				return err
			}
			if err := apply(tx); err != nil {
				return err
			}
			if want, err = renderFullConfig(tx, cert); err != nil {
				return err
			}
			return errPreviewRollback
		})
		if !errors.Is(err, errPreviewRollback) {
			return err
		}
		for round := 1; ; round++ {
			if err := dryRunFullConfig(want); err != nil {
				return err
			}
			var moved []byte
			var movedVer int64
			err := conn.Transaction(func(tx *gorm.DB) error {
				now, err := dataVersion(tx)
				if err != nil {
					return err
				}
				if err := apply(tx); err != nil {
					return err
				}
				if now == ver {
					return nil // 校验期间没有别的连接提交过:库和预演时一模一样,写进去的就是校验过的那份
				}
				raw, err := renderFullConfig(tx, cert)
				if err != nil {
					return err
				}
				if bytes.Equal(raw, want) {
					return nil
				}
				if round >= commitRounds {
					return dryRunFullConfig(raw)
				}
				moved, movedVer = raw, now
				return errConfigMoved
			})
			if !errors.Is(err, errConfigMoved) {
				return err
			}
			want, ver = moved, movedVer
		}
	})
}

// dataVersion 这条连接看到的库版本:别的连接每提交一次就变(本连接自己的改动不算)。
func dataVersion(tx *gorm.DB) (int64, error) {
	var v int64
	if err := tx.Raw("PRAGMA data_version").Scan(&v).Error; err != nil {
		return 0, fmt.Errorf("读取库版本: %w", err)
	}
	return v, nil
}

func renderFullConfig(tx *gorm.DB, cert render.NodeCert) ([]byte, error) {
	raw, err := render.BuildConfig(tx, cert)
	if err != nil {
		return nil, fmt.Errorf("渲染配置失败(已取消保存): %w", err)
	}
	return raw, nil
}

func dryRunFullConfig(raw []byte) error {
	if err := core.ValidateConfig(raw); err != nil {
		return fmt.Errorf("配置未通过 sing-box 校验(已取消保存): %w", err)
	}
	return nil
}
