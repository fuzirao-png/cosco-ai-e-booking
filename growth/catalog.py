from dataclasses import dataclass
from typing import Literal

from pydantic import BaseModel, Field


class Lead(BaseModel):
    id: str
    company: str
    contact: str
    stage: Literal["new", "registered", "verified"]
    note: str


LEADS = {
    lead.id: lead for lead in (
        Lead(id="harbor", company="海岚国际货运", contact="林经理", stage="new",
             note="关注华东至东南亚航线，首次使用线上订舱。"),
        Lead(id="meridian", company="子午线供应链", contact="陈经理", stage="verified",
             note="已完成资质认证，近期询问宁波至洛杉矶运价。"),
        Lead(id="sail", company="远帆物流", contact="周经理", stage="registered",
             note="已注册企业账号，待完成资质认证。"),
    )
}

PORTS = {
    "CNSHA": "上海",
    "CNNGB": "宁波",
    "CNYTN": "盐田",
    "SGSIN": "新加坡",
    "USLAX": "洛杉矶",
    "NLRTM": "鹿特丹",
}


class QuoteRequest(BaseModel):
    origin: str
    destination: str
    container_type: Literal["20GP", "40GP", "40HC"]
    cargo_volume: int = Field(ge=1, le=50)


class Quote(BaseModel):
    id: str
    request: QuoteRequest
    unit_price: int
    currency: Literal["USD"] = "USD"

    @property
    def total(self) -> int:
        return self.unit_price * self.request.cargo_volume


@dataclass(frozen=True)
class Tariff:
    origin: str
    destination: str
    container_type: str
    unit_price: int

    @property
    def id(self) -> str:
        return f"rate-{self.origin}-{self.destination}-{self.container_type}"

    @property
    def text(self) -> str:
        return (
            f"{PORTS[self.origin]}({self.origin}) 至 {PORTS[self.destination]}({self.destination})，"
            f"{self.container_type} 样例海运费 USD {self.unit_price} / 箱，仅包含基础海运费。"
            "申请订舱前需确认数量及企业资质。"
        )


TARIFFS = tuple(
    Tariff(origin, destination, kind, price)
    for (origin, destination), prices in {
        ("CNSHA", "SGSIN"): (420, 660, 690),
        ("CNNGB", "USLAX"): (1480, 2080, 2210),
        ("CNYTN", "SGSIN"): (390, 590, 640),
        ("CNSHA", "NLRTM"): (1280, 1890, 1960),
    }.items()
    for kind, price in zip(("20GP", "40GP", "40HC"), prices)
)

RATES = {(rate.origin, rate.destination, rate.container_type): rate for rate in TARIFFS}
