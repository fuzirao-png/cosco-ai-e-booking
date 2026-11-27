package growth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

type Lead struct {
	ID      string `json:"id"`
	Company string `json:"company"`
	Contact string `json:"contact"`
	Stage   string `json:"stage"`
	Note    string `json:"note"`
}

var leads = []Lead{
	{"harbor", "海岚国际货运", "林经理", "new", "关注华东至东南亚航线，首次使用线上订舱。"},
	{"meridian", "子午线供应链", "陈经理", "verified", "已完成资质认证，近期询问宁波至洛杉矶运价。"},
	{"sail", "远帆物流", "周经理", "registered", "已注册企业账号，待完成资质认证。"},
}

func findLead(id string) (Lead, bool) {
	for _, lead := range leads {
		if lead.ID == id {
			return lead, true
		}
	}
	return Lead{}, false
}

var ports = []struct{ code, name string }{
	{"CNSHA", "上海"}, {"CNNGB", "宁波"}, {"CNYTN", "盐田"},
	{"SGSIN", "新加坡"}, {"USLAX", "洛杉矶"}, {"NLRTM", "鹿特丹"},
}

func portName(code string) string {
	for _, port := range ports {
		if port.code == code {
			return port.name
		}
	}
	return code
}

type QuoteRequest struct {
	Origin        string `json:"origin"`
	Destination   string `json:"destination"`
	ContainerType string `json:"container_type"`
	CargoVolume   int    `json:"cargo_volume"`
}

type Quote struct {
	ID        string       `json:"id"`
	Request   QuoteRequest `json:"request"`
	UnitPrice int          `json:"unit_price"`
	Currency  string       `json:"currency"`
}

func (q Quote) total() int { return q.UnitPrice * q.Request.CargoVolume }

type tariff struct {
	origin, destination, kind string
	price                     int
}

func (t tariff) id() string {
	return strings.Join([]string{"rate", t.origin, t.destination, t.kind}, "-")
}
func (t tariff) body() string {
	return fmt.Sprintf("%s(%s) 至 %s(%s)，%s 样例海运费 USD %d / 箱，仅包含基础海运费。申请订舱前需确认数量及企业资质。",
		portName(t.origin), t.origin, portName(t.destination), t.destination, t.kind, t.price)
}

var tariffs = []tariff{
	{"CNSHA", "SGSIN", "20GP", 420}, {"CNSHA", "SGSIN", "40GP", 660}, {"CNSHA", "SGSIN", "40HC", 690},
	{"CNNGB", "USLAX", "20GP", 1480}, {"CNNGB", "USLAX", "40GP", 2080}, {"CNNGB", "USLAX", "40HC", 2210},
	{"CNYTN", "SGSIN", "20GP", 390}, {"CNYTN", "SGSIN", "40GP", 590}, {"CNYTN", "SGSIN", "40HC", 640},
	{"CNSHA", "NLRTM", "20GP", 1280}, {"CNSHA", "NLRTM", "40GP", 1890}, {"CNSHA", "NLRTM", "40HC", 1960},
}

func newID(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func shorten(text string, size int) string {
	runes := []rune(text)
	if len(runes) > size {
		return string(runes[:size])
	}
	return text
}

func stringPtr(value string) *string { return &value }
