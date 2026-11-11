package model

import "strings"

// BoolSettings 取值只能是开或关的设置项:设置页的勾选框(bool / boolOn)、主副机角色、备份推送。
// 保存时统一规整成 "true" / "false"(见 NormBoolSetting):以前存成 0 / off / no 时,设置页按"不是 true 就是关"显示,
// 后端有的按"不是 false 就是开"判断,界面显示关、实际却开着。设置页加勾选框时要同步加到这里(web 的测试会核对)。
var BoolSettings = map[string]bool{
	"supportOnline": true, "subUseUserName": true, "subEncode": true, "subShowNotice": true, "subAutoFormat": true,
	"resellerEnabled": true, "subPageEnabled": true, "subShareEnabled": true, "subPageBrand": true,
	"tgEnabled": true, "tgBot": true, "tgOnLogin": true, "tgOnUserDisabled": true, "tgOnUserExpiring": true,
	"tgOnQuota": true, "tgOnRuleLimit": true, "tgOnUpstream": true, "tgOnCore": true, "tgOnReach": true, "tgOnCert": true, "tgDaily": true,
	"reachAuto": true, "allowPrivate": true, "blockBT": true, "nodeMode": true, "backupTelegram": true,
	"haWitness":    true, // 主机容灾的境外测点见证(在主机容灾设置里改;命令行 m-ui set 写它也规整)
	"haAllowPlain": true, // 主机容灾:允许明文链接的副机当备用
}

// NormBoolSetting 把开关类设置的写法规整成 "true" / "false";空值原样保留(表示用默认值)。ok=false 表示认不出。
func NormBoolSetting(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return "", true
	case "true", "1", "on", "yes":
		return "true", true
	case "false", "0", "off", "no":
		return "false", true
	}
	return "", false
}
