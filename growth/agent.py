import re
from dataclasses import dataclass, field
from uuid import uuid4

from .catalog import LEADS, PORTS, RATES, Quote, QuoteRequest
from .knowledge import Chunk, KnowledgeBase
from .store import Evidence, Profile, ProfileUpdate, Store


@dataclass(frozen=True)
class Skill:
    intent_label: str
    trigger_intent: tuple[str, ...]
    required_slots: tuple[str, ...] = ()


QUOTE = Skill("quote", ("查价", "运价", "报价", "多少钱", "海运费"),
              ("origin", "destination", "container_type", "cargo_volume"))
SKILLS = (
    Skill("opt_out", ("不要再打", "请勿来电", "拒绝外呼", "不要联系", "别再打", "停止联系")),
    Skill("status", ("认证状态", "注册状态", "企业状态", "当前进度")),
    Skill("register", ("确认注册",), ("company",)),
    Skill("verify", ("确认认证", "提交认证")),
    Skill("confirm", ("确认订舱",), ("quote_id",)),
    Skill("book", ("申请订舱", "提交订舱", "订舱预览")),
    QUOTE,
)
SLOT_LABELS = {"origin": "起运港", "destination": "目的港",
               "container_type": "箱型（20GP、40GP 或 40HC）", "cargo_volume": "箱量"}


@dataclass
class Session:
    lead_id: str
    call_seq: int
    profile: Profile
    id: str = field(default_factory=lambda: uuid4().hex)
    epoch: int = 0
    utterance_seq: int = 0
    closed: bool = False
    slots: dict = field(default_factory=dict)
    quote: Quote | None = None
    proposal: Quote | None = None
    partial: str = ""
    restricted_cargo: bool = False
    transcript: list[Evidence] = field(default_factory=list)
    updates: dict[str, ProfileUpdate] = field(default_factory=dict)

    def begin(self, text: str) -> int:
        self.epoch += 1
        self.utterance_seq += 1
        self.partial = ""
        if re.search(r"认证|卡在|卡点是|难点是", text) and not sensitive(text):
            self.transcript.append(Evidence(call_seq=self.call_seq,
                                            utterance_seq=self.utterance_seq, quote=text[:500]))
        return self.epoch

    def remember(self, field_name: str, value: str | None, quote: str) -> None:
        if sensitive(quote):
            return
        self.updates[field_name] = ProfileUpdate(
            field=field_name, value=value,
            evidence=Evidence(call_seq=self.call_seq, utterance_seq=self.utterance_seq,
                              quote=quote[:500]),
        )


@dataclass
class Reply:
    text: str
    intent: str
    status: str = "ok"
    data: dict = field(default_factory=dict)
    sources: list[Chunk] = field(default_factory=list)


def sensitive(text: str) -> bool:
    return bool(re.search(r"密码|验证码|系统提示词|忽略.{0,8}指令|\d{11,}", text))


def explicit_intent(text: str, session: Session) -> str | None:
    for skill in SKILLS:
        if not any(trigger in text for trigger in skill.trigger_intent):
            continue
        if skill.intent_label in {"register", "verify", "confirm", "book"}:
            if not any(text.startswith(trigger) for trigger in skill.trigger_intent):
                continue
        if skill.intent_label == "quote":
            pricing_request = bool(extract_slots(text)) or text in (
                "查价", "查询运价", "报价", "我想查价", "我要查价",
            )
            if not pricing_request:
                continue
        return skill.intent_label
    if session.slots and not session.quote and extract_slots(text):
        return "quote"
    return None


def extract_slots(text: str) -> dict:
    matches = []
    for code, name in PORTS.items():
        match = re.search(rf"{code}|{name}", text, re.IGNORECASE)
        if match:
            matches.append((match.start(), code))
    ports = [code for _, code in sorted(matches)]
    kind = re.search(r"(?<![a-z0-9])(?:20GP|40GP|40HC|40HQ)(?![a-z])", text.upper())
    volume = re.search(r"(?<![\d.-])(-?\d+(?:\.\d+)?)\s*(?:箱|柜|个)", text)
    slots = {}
    if len(ports) > 1:
        slots.update(origin=ports[0], destination=ports[1])
    elif ports:
        port = ports[0]
        prefix = text[:matches[0][0]]
        slot = "destination" if re.search(r"(?:目的港[：: ]*|到|至)$", prefix) else "origin"
        slots[slot] = port
    if kind:
        slots["container_type"] = kind.group().replace("40HQ", "40HC")
    if volume:
        slots["cargo_volume"] = 0 if "." in volume.group(1) else int(volume.group(1))
    return slots


class Agent:
    def __init__(self, store: Store, index: KnowledgeBase):
        self.store = store
        self.index = index

    def respond(self, session: Session, text: str, intent: str) -> Reply:
        stage, _ = self.store.get_lead(session.lead_id)
        if intent == "opt_out":
            self.store.opt_out(session.lead_id)
            return Reply("已记录停止联系的请求，后续演示外呼会跳过此线索。", intent)
        if sensitive(text):
            return Reply("请勿在通话中提供密码、验证码或证件号码。请通过企业账户办理。",
                         "knowledge", "guarded")
        if re.search(r"危险品|易燃|爆炸品|锂电池", text):
            session.restricted_cargo = True
        if session.restricted_cargo and intent in {"quote", "book", "confirm"}:
            session.proposal = None
            return Reply("这类货物需要如实申报并经人工合规审核。请联系承运人核实。", intent, "blocked")
        match intent:
            case "status":
                label = {"new": "待注册", "registered": "待认证", "verified": "已认证"}[stage]
                return Reply(f"当前企业状态为{label}。", intent, data={"stage": stage})
            case "register":
                company = LEADS[session.lead_id].company
                if text != f"确认注册 {company}":
                    return Reply(f"请核对企业名称，并输入「确认注册 {company}」。",
                                 intent, "needs_input")
                if stage == "new":
                    self.store.advance(session.lead_id, "new", "registered")
                return Reply("演示企业注册完成。请继续进行资质认证。", intent,
                             data={"stage": self.store.get_lead(session.lead_id)[0]})
            case "verify":
                if stage == "new":
                    return Reply("请先完成企业注册，再进行资质认证。", intent, "blocked")
                if text != "确认认证":
                    return Reply("请确认在演示中提交资质材料，输入「确认认证」。", intent, "needs_input")
                if stage == "registered":
                    self.store.advance(session.lead_id, "registered", "verified")
                session.remember("current_blocker", None, text)
                return Reply("演示资质审核通过。现在可以查价并申请订舱。", intent,
                             data={"stage": "verified"})
            case "quote":
                return self.quote(session, text)
            case "book" | "confirm":
                return self.booking(session, text, stage, intent)
            case _:
                return self.knowledge(session, text)

    def quote(self, session: Session, text: str) -> Reply:
        updates = extract_slots(text)
        if session.quote:
            session.slots.clear()
        session.slots.update(updates)
        session.quote = session.proposal = None
        missing = [SLOT_LABELS[slot] for slot in QUOTE.required_slots if slot not in session.slots]
        if missing:
            return Reply("请补充" + "、".join(missing) + "。例如「上海到新加坡 40HC 2箱」。",
                         "quote", "needs_input", {"slots": session.slots.copy()})
        if not 1 <= session.slots["cargo_volume"] <= 50:
            return Reply("箱量需在 1 至 50 之间，请重新输入箱量。", "quote", "needs_input")
        request = QuoteRequest.model_validate(session.slots)
        tariff = RATES.get((request.origin, request.destination, request.container_type))
        if tariff is None:
            return Reply("该航线和箱型暂无演示运价。可尝试上海至新加坡或宁波至洛杉矶。",
                         "quote", "not_found")
        quote = Quote(id=uuid4().hex[:12].upper(), request=request, unit_price=tariff.unit_price)
        session.quote = quote
        route = f"{PORTS[request.origin]} → {PORTS[request.destination]}"
        session.remember("route_interest", route, text)
        data = {**quote.model_dump(), "total": quote.total, "route": route}
        sources = [chunk for chunk in self.index.documents if chunk.id == tariff.id]
        return Reply(f"{route}，{request.container_type} × {request.cargo_volume}。"
                     f"样例海运费 USD {quote.unit_price:,} / 箱，合计 USD {quote.total:,}。"
                     "如需继续，请说「申请订舱」。", "quote", data=data, sources=sources)

    def booking(self, session: Session, text: str, stage: str, intent: str) -> Reply:
        if stage != "verified":
            return Reply("提交订舱前需完成企业资质认证。", intent, "blocked")
        if intent == "book":
            if text != "申请订舱":
                return Reply("输入「申请订舱」可生成预览。", intent, "needs_input")
            if session.quote is None:
                return Reply("请先查询航线、箱型及箱量对应的运价。", intent, "needs_input")
            session.proposal = quote = session.quote
            request = quote.request
            return Reply(f"订舱预览 {quote.id}。{PORTS[request.origin]} → {PORTS[request.destination]}，"
                         f"{request.container_type} × {request.cargo_volume}，"
                         f"样例海运费 USD {quote.total:,}。"
                         f"核对后请输入「确认订舱 {quote.id}」。", intent, "preview",
                         {"confirmation": f"确认订舱 {quote.id}"})
        quote = session.proposal
        if quote is None or text != f"确认订舱 {quote.id}":
            return Reply("请先生成订舱预览，再使用预览中的确认编号。", intent, "needs_input")
        reference = self.store.book(session.lead_id, quote)
        return Reply(f"演示订舱已提交，编号 {reference}。", intent, data={"reference": reference})

    def knowledge(self, session: Session, text: str) -> Reply:
        if session.profile.current_blocker and re.search(r"继续|上次", text):
            return Reply(f"上次的卡点是「{session.profile.current_blocker}」。"
                         "这次可以继续处理这个问题。", "memory",
                         data={"profile_revision": session.profile.revision})
        sources = self.index.search(text)
        answer = sources[0].raw_text if sources else "演示资料中没有找到对应信息，请补充具体的订舱问题。"
        return Reply(answer, "knowledge", sources=sources)
