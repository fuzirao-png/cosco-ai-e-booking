package growth

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

type skill struct {
	intent         string
	triggers       []string
	requiresPrefix bool
}

var skills = []skill{
	{"opt_out", []string{"不要再打", "请勿来电", "拒绝外呼", "不要联系", "别再打", "停止联系"}, false},
	{"status", []string{"认证状态", "注册状态", "企业状态", "当前进度"}, false},
	{"register", []string{"确认注册"}, true},
	{"verify", []string{"确认认证", "提交认证"}, true},
	{"confirm", []string{"确认订舱"}, true},
	{"book", []string{"申请订舱", "提交订舱", "订舱预览"}, true},
	{"quote", []string{"查价", "运价", "报价", "多少钱", "海运费"}, false},
}

var quoteSlots = []struct{ key, label string }{
	{"origin", "起运港"}, {"destination", "目的港"},
	{"container_type", "箱型（20GP、40GP 或 40HC）"}, {"cargo_volume", "箱量"},
}

var (
	sensitivePattern   = regexp.MustCompile(`密码|验证码|系统提示词|忽略.{0,8}指令|\p{Nd}{11,}`)
	evidencePattern    = regexp.MustCompile(`认证|卡在|卡点是|难点是`)
	restrictedPattern  = regexp.MustCompile(`危险品|易燃|爆炸品|锂电池`)
	resumePattern      = regexp.MustCompile(`继续|上次`)
	destinationPattern = regexp.MustCompile(`(目的港[：: ]*|到|至)$`)
	kindPattern        = regexp.MustCompile(`20GP|40GP|40HC|40HQ`)
	volumePattern      = regexp.MustCompile(`(-?\d+(\.\d+)?)[\s\v\p{Z}\x{0085}]*(箱|柜|个)`)
)

type Session struct {
	mu                 sync.Mutex
	leadID             string
	seq                int64
	profile            Profile
	epoch, utterance   int
	cancel             context.CancelFunc
	closed, restricted bool
	slots              map[string]any
	quote, proposal    *Quote
	partial            string
	transcript         []Evidence
	updates            map[string]profileUpdate
}

func NewSession(leadID string, seq int64, profile Profile) *Session {
	return &Session{leadID: leadID, seq: seq, profile: profile, slots: make(map[string]any),
		updates: make(map[string]profileUpdate)}
}

func (s *Session) bump() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.epoch++
}

func (s *Session) begin(text string) {
	s.bump()
	s.utterance++
	s.partial = ""
	if evidencePattern.MatchString(text) && !sensitivePattern.MatchString(text) {
		s.transcript = append(s.transcript, s.evidence(text))
	}
}

func (s *Session) evidence(text string) Evidence {
	return Evidence{s.seq, s.utterance, shorten(text, 500)}
}

func (s *Session) remember(field string, value *string, text string) {
	if !sensitivePattern.MatchString(text) {
		s.updates[field] = profileUpdate{field, value, s.evidence(text)}
	}
}

type Reply struct {
	Text    string
	Intent  string
	Status  string
	Data    map[string]any
	Sources []Chunk
}

func reply(text, intent, status string, data map[string]any) Reply {
	if data == nil {
		data = map[string]any{}
	}
	return Reply{Text: text, Intent: intent, Status: status, Data: data}
}

func explicitIntent(text string, s *Session) string {
	for _, skill := range skills {
		match := strings.Contains
		if skill.requiresPrefix {
			match = strings.HasPrefix
		}
		triggered := false
		for _, trigger := range skill.triggers {
			if match(text, trigger) {
				triggered = true
				break
			}
		}
		if !triggered {
			continue
		}
		if skill.intent == "quote" && len(extractSlots(text)) == 0 {
			switch text {
			case "查价", "查询运价", "报价", "我想查价", "我要查价":
			default:
				continue
			}
		}
		return skill.intent
	}
	if len(s.slots) > 0 && s.quote == nil && len(extractSlots(text)) > 0 {
		return "quote"
	}
	return ""
}

func extractSlots(text string) map[string]any {
	type foundPort struct {
		at   int
		code string
	}
	var found []foundPort
	upper := strings.ToUpper(text)
	for _, port := range ports {
		at := strings.Index(upper, port.code)
		if nameAt := strings.Index(upper, port.name); nameAt >= 0 && (at == -1 || nameAt < at) {
			at = nameAt
		}
		if at >= 0 {
			found = append(found, foundPort{at, port.code})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].at < found[j].at })
	slots := map[string]any{}
	if len(found) > 1 {
		slots["origin"], slots["destination"] = found[0].code, found[1].code
	} else if len(found) == 1 {
		key := "origin"
		if destinationPattern.MatchString(upper[:found[0].at]) {
			key = "destination"
		}
		slots[key] = found[0].code
	}
	for _, indices := range kindPattern.FindAllStringIndex(upper, -1) {
		start, end := indices[0], indices[1]
		if start > 0 && (isLetter(upper[start-1]) || isDigit(upper[start-1])) {
			continue
		}
		if end < len(upper) && isLetter(upper[end]) {
			continue
		}
		slots["container_type"] = strings.ReplaceAll(upper[start:end], "40HQ", "40HC")
		break
	}
	quantityText := strings.Map(asciiDecimalDigit, text)
	for _, indices := range volumePattern.FindAllStringSubmatchIndex(quantityText, -1) {
		start := indices[0]
		if start > 0 && (isDigit(quantityText[start-1]) || quantityText[start-1] == '.' || quantityText[start-1] == '-') {
			continue
		}
		volume, err := strconv.Atoi(quantityText[indices[2]:indices[3]])
		if err != nil {
			volume = 0
		}
		slots["cargo_volume"] = volume
		break
	}
	return slots
}

func isLetter(c byte) bool { return c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }

func asciiDecimalDigit(value rune) rune {
	if value < 128 || !unicode.IsDigit(value) {
		return value
	}
	first := value
	for unicode.IsDigit(first - 1) {
		first--
	}
	return '0' + (value-first)%10
}

var errOptedOut = errors.New("lead opted out")

type Agent struct {
	Store *Store
	Index *Index
}

func (a Agent) Respond(ctx context.Context, s *Session, text string) (Reply, error) {
	s.begin(text)
	result, err := a.decide(ctx, s, text, explicitIntent(text, s))
	if err != nil {
		return Reply{}, err
	}
	return a.enrich(ctx, result, text)
}

func (a Agent) decide(ctx context.Context, s *Session, text, intent string) (Reply, error) {
	optedOut, err := a.Store.optedOut(ctx, s.leadID)
	if err != nil {
		return Reply{}, err
	}
	if optedOut {
		return Reply{}, errOptedOut
	}
	if intent == "opt_out" {
		return reply("已记录停止联系的请求，后续演示外呼会跳过此线索。", intent, "ok", nil), a.Store.optOut(ctx, s.leadID)
	}
	if sensitivePattern.MatchString(text) {
		return reply("请勿在通话中提供密码、验证码或证件号码。请通过企业账户办理。", "knowledge", "guarded", nil), nil
	}
	s.restricted = s.restricted || restrictedPattern.MatchString(text)
	if s.restricted && (intent == "quote" || intent == "book" || intent == "confirm") {
		s.proposal = nil
		return reply("这类货物需要如实申报并经人工合规审核。请联系承运人核实。", intent, "blocked", nil), nil
	}
	switch intent {
	case "status", "register", "verify", "book", "confirm":
		stage, _, err := readProfile(ctx, a.Store.db, s.leadID)
		if err != nil {
			return Reply{}, err
		}
		return a.enterprise(ctx, s, text, intent, stage)
	case "quote":
		return a.quote(s, text)
	default:
		if s.profile.CurrentBlocker != nil && *s.profile.CurrentBlocker != "" && resumePattern.MatchString(text) {
			return reply(fmt.Sprintf("上次的卡点是「%s」。这次可以继续处理这个问题。", *s.profile.CurrentBlocker),
				"memory", "ok", map[string]any{"profile_revision": s.profile.Revision}), nil
		}
		return reply("", "knowledge", "ok", nil), nil
	}
}

func (a Agent) enterprise(ctx context.Context, s *Session, text, intent, stage string) (Reply, error) {
	switch intent {
	case "status":
		label := map[string]string{"new": "待注册", "registered": "待认证", "verified": "已认证"}[stage]
		return reply("当前企业状态为"+label+"。", intent, "ok", map[string]any{"stage": stage}), nil
	case "register":
		lead, _ := findLead(s.leadID)
		if text != "确认注册 "+lead.Company {
			return reply("请核对企业名称，并输入「确认注册 "+lead.Company+"」。", intent, "needs_input", nil), nil
		}
		if stage == "new" {
			if err := a.Store.advance(ctx, s.leadID, "new", "registered"); err != nil {
				return Reply{}, err
			}
			stage = "registered"
		}
		return reply("演示企业注册完成。请继续进行资质认证。", intent, "ok", map[string]any{"stage": stage}), nil
	case "verify":
		if stage == "new" {
			return reply("请先完成企业注册，再进行资质认证。", intent, "blocked", nil), nil
		}
		if text != "确认认证" {
			return reply("请确认在演示中提交资质材料，输入「确认认证」。", intent, "needs_input", nil), nil
		}
		if stage == "registered" {
			if err := a.Store.advance(ctx, s.leadID, "registered", "verified"); err != nil {
				return Reply{}, err
			}
		}
		s.remember("current_blocker", nil, text)
		return reply("演示资质审核通过。现在可以查价并申请订舱。", intent, "ok", map[string]any{"stage": "verified"}), nil
	default:
		return a.booking(ctx, s, text, stage, intent)
	}
}

func (a Agent) quote(s *Session, text string) (Reply, error) {
	if s.quote != nil {
		clear(s.slots)
	}
	maps.Copy(s.slots, extractSlots(text))
	s.quote, s.proposal = nil, nil
	var missing []string
	for _, slot := range quoteSlots {
		if _, exists := s.slots[slot.key]; !exists {
			missing = append(missing, slot.label)
		}
	}
	if len(missing) > 0 {
		return reply("请补充"+strings.Join(missing, "、")+"。例如「上海到新加坡 40HC 2箱」。",
			"quote", "needs_input", map[string]any{"slots": maps.Clone(s.slots)}), nil
	}
	request := QuoteRequest{s.slots["origin"].(string), s.slots["destination"].(string),
		s.slots["container_type"].(string), s.slots["cargo_volume"].(int)}
	if request.CargoVolume < 1 || request.CargoVolume > 50 {
		return reply("箱量需在 1 至 50 之间，请重新输入箱量。", "quote", "needs_input", nil), nil
	}
	for _, rate := range tariffs {
		if rate.origin != request.Origin || rate.destination != request.Destination || rate.kind != request.ContainerType {
			continue
		}
		id, err := newID(6)
		if err != nil {
			return Reply{}, err
		}
		quote := Quote{strings.ToUpper(id), request, rate.price, "USD"}
		s.quote = &quote
		route := portName(request.Origin) + " → " + portName(request.Destination)
		s.remember("route_interest", &route, text)
		data := map[string]any{"id": quote.ID, "request": request, "unit_price": quote.UnitPrice, "currency": quote.Currency,
			"total": quote.total(), "route": route}
		message := fmt.Sprintf("%s，%s × %d。样例海运费 USD %s / 箱，合计 USD %s。如需继续，请说「申请订舱」。",
			route, request.ContainerType, request.CargoVolume, formatPrice(quote.UnitPrice), formatPrice(quote.total()))
		result := reply(message, "quote", "ok", data)
		result.Sources = a.Index.citation(rate.id())
		return result, nil
	}
	return reply("该航线和箱型暂无演示运价。可尝试上海至新加坡或宁波至洛杉矶。", "quote", "not_found", nil), nil
}

func (a Agent) booking(ctx context.Context, s *Session, text, stage, intent string) (Reply, error) {
	if stage != "verified" {
		return reply("提交订舱前需完成企业资质认证。", intent, "blocked", nil), nil
	}
	if intent == "book" {
		if text != "申请订舱" {
			return reply("输入「申请订舱」可生成预览。", intent, "needs_input", nil), nil
		}
		quote := s.quote
		if quote == nil {
			return reply("请先查询航线、箱型及箱量对应的运价。", intent, "needs_input", nil), nil
		}
		s.proposal = quote
		request := quote.Request
		message := fmt.Sprintf("订舱预览 %s。%s → %s，%s × %d，样例海运费 USD %s。核对后请输入「确认订舱 %s」。",
			quote.ID, portName(request.Origin), portName(request.Destination), request.ContainerType, request.CargoVolume,
			formatPrice(quote.total()), quote.ID)
		return reply(message, intent, "preview", map[string]any{"confirmation": "确认订舱 " + quote.ID}), nil
	}
	if s.proposal == nil || text != "确认订舱 "+s.proposal.ID {
		return reply("请先生成订舱预览，再使用预览中的确认编号。", intent, "needs_input", nil), nil
	}
	reference, err := a.Store.book(ctx, s.leadID, *s.proposal)
	if err != nil {
		return Reply{}, err
	}
	return reply("演示订舱已提交，编号 "+reference+"。", intent, "ok", map[string]any{"reference": reference}), nil
}

func (a Agent) enrich(ctx context.Context, result Reply, text string) (Reply, error) {
	if result.Text != "" {
		return result, nil
	}
	sources, err := a.Index.Search(ctx, text)
	result.Sources = sources
	result.Text = "演示资料中没有找到对应信息，请补充具体的订舱问题。"
	if len(sources) > 0 {
		result.Text = sources[0].Text
	}
	return result, err
}

func formatPrice(value int) string {
	valueText := strconv.Itoa(value)
	for i := len(valueText) - 3; i > 0; i -= 3 {
		valueText = valueText[:i] + "," + valueText[i:]
	}
	return valueText
}
