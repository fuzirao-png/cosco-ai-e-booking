import json
import re
import sqlite3
from contextlib import closing
from pathlib import Path
from typing import Literal

from pydantic import BaseModel, Field

from .catalog import LEADS, Quote


class Evidence(BaseModel):
    call_seq: int
    utterance_seq: int
    quote: str = Field(max_length=500)

    @property
    def order(self) -> tuple[int, int]:
        return self.call_seq, self.utterance_seq


class ProfileUpdate(BaseModel):
    field: Literal["current_blocker", "route_interest"]
    value: str | None
    evidence: Evidence


class Profile(BaseModel):
    schema_version: Literal[1] = 1
    revision: int = 0
    current_blocker: str | None = None
    route_interest: str | None = None
    evidence: dict[str, Evidence] = Field(default_factory=dict)


def extract_blockers(transcript: list[Evidence]) -> list[ProfileUpdate]:
    updates = []
    for evidence in transcript:
        match = re.search(r"认证.{0,12}(?:没准备好|未准备|材料不齐|有问题)|"
                          r"(?:卡在|卡点是|难点是)[：: ]?([^。！？]{1,80})", evidence.quote)
        if match:
            updates.append(ProfileUpdate(field="current_blocker",
                                         value=match.group(1) or "资质认证材料待准备",
                                         evidence=evidence))
    return updates


class Store:
    def __init__(self, path: Path):
        self.path = path
        path.parent.mkdir(parents=True, exist_ok=True)
        with closing(self.connect()) as db, db:
            db.executescript("""
                PRAGMA journal_mode = WAL;
                CREATE TABLE IF NOT EXISTS leads (
                    id TEXT PRIMARY KEY, stage TEXT NOT NULL, profile TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS calls (
                    seq INTEGER PRIMARY KEY, lead_id TEXT NOT NULL REFERENCES leads(id)
                );
                CREATE TABLE IF NOT EXISTS profile_jobs (
                    call_seq INTEGER PRIMARY KEY REFERENCES calls(seq),
                    updates TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS bookings (
                    quote_id TEXT PRIMARY KEY, lead_id TEXT NOT NULL REFERENCES leads(id),
                    reference TEXT NOT NULL UNIQUE, payload TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS outreach_opt_out (
                    lead_id TEXT PRIMARY KEY REFERENCES leads(id)
                );
            """)
            db.executemany("INSERT OR IGNORE INTO leads VALUES (?, ?, ?)", (
                (lead.id, lead.stage, Profile(
                    route_interest="宁波 → 洛杉矶" if lead.id == "meridian" else None,
                ).model_dump_json()) for lead in LEADS.values()
            ))

    def connect(self):
        db = sqlite3.connect(self.path, timeout=5)
        db.execute("PRAGMA foreign_keys = ON")
        return db

    def get_lead(self, lead_id: str) -> tuple[str, Profile]:
        with closing(self.connect()) as db:
            stage, payload = db.execute("SELECT stage, profile FROM leads WHERE id=?",
                                        (lead_id,)).fetchone()
        return stage, Profile.model_validate_json(payload)

    def open_call(self, lead_id: str) -> int:
        with closing(self.connect()) as db, db:
            return db.execute("INSERT INTO calls(lead_id) VALUES (?)", (lead_id,)).lastrowid

    def advance(self, lead_id: str, before: str, after: str) -> bool:
        with closing(self.connect()) as db, db:
            return db.execute("UPDATE leads SET stage=? WHERE id=? AND stage=?",
                              (after, lead_id, before)).rowcount == 1

    def opted_out(self, lead_id: str) -> bool:
        with closing(self.connect()) as db:
            return db.execute("SELECT 1 FROM outreach_opt_out WHERE lead_id=?",
                              (lead_id,)).fetchone() is not None

    def opt_out(self, lead_id: str) -> None:
        with closing(self.connect()) as db, db:
            db.execute("INSERT OR IGNORE INTO outreach_opt_out VALUES (?)", (lead_id,))

    def book(self, lead_id: str, quote: Quote) -> str:
        reference = "BK-" + quote.id
        with closing(self.connect()) as db, db:
            db.execute("INSERT OR IGNORE INTO bookings VALUES (?, ?, ?, ?)",
                       (quote.id, lead_id, reference, quote.model_dump_json()))
            result, owner = db.execute("SELECT reference, lead_id FROM bookings WHERE quote_id=?",
                                       (quote.id,)).fetchone()
            if owner != lead_id:
                raise ValueError("Booking owner mismatch")
        return result

    def enqueue(self, call_seq: int, updates: list[ProfileUpdate],
                transcript: list[Evidence]) -> None:
        if not updates and not transcript:
            return
        payload = json.dumps({
            "updates": [item.model_dump() for item in updates],
            "transcript": [item.model_dump() for item in transcript],
        }, ensure_ascii=False)
        with closing(self.connect()) as db, db:
            db.execute("INSERT OR IGNORE INTO profile_jobs VALUES (?, ?)", (call_seq, payload))

    def apply_pending(self, for_lead: str | None = None) -> int:
        with closing(self.connect()) as db, db:
            db.execute("BEGIN IMMEDIATE")
            jobs = db.execute("""
                SELECT c.seq, c.lead_id, j.updates FROM profile_jobs j
                JOIN calls c ON c.seq = j.call_seq
                WHERE (? IS NULL OR c.lead_id = ?)
                ORDER BY c.seq
            """, (for_lead, for_lead)).fetchall()
            for call_seq, lead_id, payload in jobs:
                raw = db.execute("SELECT profile FROM leads WHERE id=?", (lead_id,)).fetchone()[0]
                profile = Profile.model_validate_json(raw)
                changed = False
                job = json.loads(payload)
                updates = [ProfileUpdate.model_validate(item) for item in job["updates"]]
                updates.extend(extract_blockers([Evidence.model_validate(item)
                                                 for item in job["transcript"]]))
                for update in sorted(updates, key=lambda item: item.evidence.order):
                    previous = profile.evidence.get(update.field)
                    if previous is None or update.evidence.order > previous.order:
                        setattr(profile, update.field, update.value)
                        profile.evidence[update.field] = update.evidence
                        changed = True
                if changed:
                    profile.revision += 1
                    db.execute("UPDATE leads SET profile=? WHERE id=?",
                               (profile.model_dump_json(), lead_id))
                db.execute("DELETE FROM profile_jobs WHERE call_seq=?", (call_seq,))
            return len(jobs)
