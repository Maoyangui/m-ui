package reach

import (
	"regexp"
	"sort"
	"strings"
)

// 运营商分组。顺序就是界面与通知里的顺序。
const (
	GroupMobile  = "cm" // 中国移动
	GroupTelecom = "ct" // 中国电信
	GroupUnicom  = "cu" // 中国联通
	GroupAbroad  = "hk" // 香港对照组:境外能通、大陆不通,才说得上是"被墙"
)

var carrierGroups = []string{GroupMobile, GroupTelecom, GroupUnicom}

// 三家的骨干与各省网 ASN。列表之外的再按网络名认(Globalping 给的 network 是 ASN 登记名)。
var carrierASN = map[int]string{
	// 移动
	9808: GroupMobile, 24400: GroupMobile, 24444: GroupMobile, 24445: GroupMobile, 24547: GroupMobile,
	38019: GroupMobile, 56040: GroupMobile, 56041: GroupMobile, 56042: GroupMobile, 56044: GroupMobile,
	56046: GroupMobile, 56047: GroupMobile, 56048: GroupMobile, 132525: GroupMobile, 134810: GroupMobile,
	// 电信
	4134: GroupTelecom, 4809: GroupTelecom, 4811: GroupTelecom, 4812: GroupTelecom, 4813: GroupTelecom,
	4816: GroupTelecom, 4835: GroupTelecom, 17799: GroupTelecom, 23650: GroupTelecom, 23724: GroupTelecom,
	134420: GroupTelecom, 134764: GroupTelecom, 134768: GroupTelecom, 134771: GroupTelecom, 140292: GroupTelecom,
	// 联通
	4837: GroupUnicom, 4808: GroupUnicom, 9929: GroupUnicom, 17621: GroupUnicom, 17622: GroupUnicom,
	17623: GroupUnicom, 17638: GroupUnicom, 17816: GroupUnicom, 136958: GroupUnicom,
}

var (
	reMobile  = regexp.MustCompile(`(?i)china\s*mobile|mobile\s+communications?`)
	reTelecom = regexp.MustCompile(`(?i)chinanet|china\s*telecom`)
	reUnicom  = regexp.MustCompile(`(?i)unicom|china169|cncgroup`)
)

// carrierOf 大陆测点属于哪家运营商;不是三家的返回空。
func carrierOf(asn int, network string) string {
	if g, ok := carrierASN[asn]; ok {
		return g
	}
	switch {
	case reMobile.MatchString(network):
		return GroupMobile
	case reTelecom.MatchString(network):
		return GroupTelecom
	case reUnicom.MatchString(network):
		return GroupUnicom
	}
	return ""
}

// eyeballTag 家庭宽带 / 移动网络测点;机房里的测点带 datacenter-network。
// 用户明确要求机房测点不算:云厂商机房走的是机房线路,和普通用户的家宽、手机网络不是一回事。
const eyeballTag = "eyeball-network"

// abroadLocations 境外对照组:香港任选 3 个测点(机房的也行,只用来确认服务器本身通)。
var abroadLocations = []location{{Country: "HK", Limit: 3}}

// pickLocations 从测点列表里给每家运营商挑至多 per 个家宽测点,优先不同城市;返回选点条件与每家计划的个数。
// 同一城市同一 ASN 的测点在选点条件里用 limit 合并。
func pickLocations(list []probe, per int) ([]location, map[string]int) {
	type key struct {
		asn  int
		city string
	}
	pool := map[string]map[key]int{}
	for _, p := range list {
		if p.Location.Country != "CN" || !p.hasTag(eyeballTag) {
			continue
		}
		g := carrierOf(p.Location.ASN, p.Location.Network)
		if g == "" {
			continue
		}
		if pool[g] == nil {
			pool[g] = map[key]int{}
		}
		pool[g][key{p.Location.ASN, p.Location.City}]++
	}
	var locs []location
	planned := map[string]int{}
	for _, g := range carrierGroups {
		keys := make([]key, 0, len(pool[g]))
		for k := range pool[g] {
			keys = append(keys, k)
		}
		// 测点多的城市排前面(更可能在线),其余按名字排,结果稳定
		sort.Slice(keys, func(i, j int) bool {
			a, b := pool[g][keys[i]], pool[g][keys[j]]
			if a != b {
				return a > b
			}
			if keys[i].city != keys[j].city {
				return strings.ToLower(keys[i].city) < strings.ToLower(keys[j].city)
			}
			return keys[i].asn < keys[j].asn
		})
		take := map[key]int{}
		for n := 0; n < per; {
			progressed := false
			for _, k := range keys {
				if n >= per {
					break
				}
				if take[k] < pool[g][k] {
					take[k]++
					n++
					progressed = true
				}
			}
			if !progressed {
				break
			}
		}
		for _, k := range keys {
			if take[k] > 0 {
				locs = append(locs, location{Country: "CN", City: k.city, ASN: k.asn, Tags: []string{eyeballTag}, Limit: take[k]})
				planned[g] += take[k]
			}
		}
	}
	return locs, planned
}
