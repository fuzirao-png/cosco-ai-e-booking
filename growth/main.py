import asyncio
import json
import logging
import os
import re
from contextlib import asynccontextmanager, suppress
from pathlib import Path

import httpx
from dotenv import load_dotenv
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import StreamingResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field

from .agent import Agent, Session, explicit_intent, sensitive
from .catalog import LEADS
from .knowledge import default_index
from .model import ChatModel
from .store import Store

LOG = logging.getLogger(__name__)
MODEL_ERRORS = (httpx.HTTPError, KeyError, TypeError, ValueError, IndexError)


class OpenCall(BaseModel):
    lead_id: str


class TurnInput(BaseModel):
    asr_text: str = Field(min_length=1, max_length=1200)


class Interrupt(BaseModel):
    asr_partial_token: str = Field(default="", max_length=1200)


def frame(event_type: str, epoch: int, **payload) -> str:
    data = json.dumps({"epoch": epoch, **payload}, ensure_ascii=False, separators=(",", ":"))
    return f"event: {event_type}\ndata: {data}\n\n"


def create_app(db_path: Path | None = None) -> FastAPI:
    load_dotenv()

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        app.state.store = Store(db_path or Path(os.getenv("GROWTH_DB_PATH", "runtime/growth.sqlite3")))
        app.state.agent = Agent(app.state.store, await asyncio.to_thread(default_index))
        app.state.model = ChatModel.from_env()
        app.state.sessions = {}
        app.state.work_ready = asyncio.Event()

        async def run_worker():
            while True:
                app.state.work_ready.clear()
                try:
                    await asyncio.to_thread(app.state.store.apply_pending)
                except Exception:
                    LOG.exception("Could not apply queued profile updates")
                with suppress(TimeoutError):
                    await asyncio.wait_for(app.state.work_ready.wait(), timeout=2)

        worker = asyncio.create_task(run_worker())
        try:
            yield
        finally:
            worker.cancel()
            with suppress(asyncio.CancelledError):
                await worker

    api = FastAPI(title="E-Booking Growth Agent", lifespan=lifespan)

    def session_for(request: Request, session_id: str) -> Session:
        session = request.app.state.sessions.get(session_id)
        if session is None or session.closed:
            raise HTTPException(404, "该通话已结束或不存在")
        return session

    @api.get("/api/health")
    async def health(request: Request):
        return {"status": "ok", "model": "configured" if request.app.state.model else "local",
                "embedding": os.getenv("GROWTH_EMBEDDINGS", "local")}

    @api.get("/api/leads")
    async def leads(request: Request):
        store = request.app.state.store
        result = []
        for lead in LEADS.values():
            stage, profile = store.get_lead(lead.id)
            result.append({**lead.model_dump(), "stage": stage, "profile": profile,
                           "opted_out": store.opted_out(lead.id)})
        return result

    @api.post("/api/calls", status_code=201)
    async def open_call(body: OpenCall, request: Request):
        lead = LEADS.get(body.lead_id)
        if lead is None:
            raise HTTPException(404, "未找到线索")
        store = request.app.state.store
        await asyncio.to_thread(store.apply_pending, lead.id)
        if store.opted_out(lead.id):
            raise HTTPException(409, "该线索已选择停止联系")
        if len(request.app.state.sessions) >= 64:
            raise HTTPException(429, "演示通话已满，请结束已有通话")
        _, profile = store.get_lead(lead.id)
        session = Session(lead.id, store.open_call(lead.id), profile)
        request.app.state.sessions[session.id] = session
        greeting = f"{lead.contact}您好，我是 E-Booking 演示助手，可以协助您查价、认证和订舱。"
        if profile.current_blocker:
            greeting += f"上次您提到「{profile.current_blocker}」，我们可以接着处理。"
        return {"id": session.id, "greeting": greeting, "profile": profile, "epoch": session.epoch}

    @api.post("/api/calls/{session_id}/interrupt")
    async def interrupt(session_id: str, body: Interrupt, request: Request):
        session = session_for(request, session_id)
        session.epoch += 1
        session.partial = body.asr_partial_token
        return {"epoch": session.epoch, "vad_state": "speech_started"}

    @api.post("/api/calls/{session_id}/end")
    async def end_call(session_id: str, request: Request):
        session = session_for(request, session_id)
        request.app.state.store.enqueue(session.call_seq, list(session.updates.values()), session.transcript)
        session.closed = True
        session.epoch += 1
        del request.app.state.sessions[session_id]
        request.app.state.work_ready.set()
        return {"status": "ended", "profile_update_queued": bool(session.updates or session.transcript)}

    @api.post("/api/calls/{session_id}/turns")
    async def turn(session_id: str, body: TurnInput, request: Request):
        session = session_for(request, session_id)
        asr_text = body.asr_text.strip()
        if not asr_text:
            raise HTTPException(422, "请输入有效内容")
        if request.app.state.store.opted_out(session.lead_id):
            raise HTTPException(409, "该线索已选择停止联系，请结束通话")
        if session.utterance_seq >= 100:
            raise HTTPException(429, "已达到此通话的轮次上限，请结束通话")
        epoch = session.begin(asr_text)
        utterance_seq = session.utterance_seq
        model = request.app.state.model
        agent = request.app.state.agent

        def active() -> bool:
            return session.epoch == epoch and not session.closed

        async def events():
            yield frame("turn", epoch, utterance_seq=utterance_seq)
            intent = explicit_intent(asr_text, session)
            if intent is None and model and active() and not sensitive(asr_text):
                try:
                    intent = await model.choose_read_skill(asr_text)
                except MODEL_ERRORS:
                    yield frame("notice", epoch, message="模型路由暂不可用，已切换到本地流程。")
            if not active():
                return
            # No await between the epoch check and executing a local business operation.
            if request.app.state.store.opted_out(session.lead_id):
                yield frame("notice", epoch, message="该线索已选择停止联系，请结束通话。")
                yield frame("done", epoch)
                return
            reply = agent.respond(session, asr_text, intent or "knowledge")
            yield frame("tool", epoch, intent=reply.intent, status=reply.status,
                        result=reply.data, sources=[source.citation() for source in reply.sources])
            if model and reply.intent == "knowledge" and reply.sources:
                evidence = "\n".join(f"[{item.id}] {item.raw_text}" for item in reply.sources)[:3600]
                emitted = False
                try:
                    async for token in model.stream_knowledge(asr_text, evidence):
                        if not active():
                            return
                        emitted = True
                        yield frame("delta", epoch, text=token)
                except MODEL_ERRORS:
                    if active():
                        yield frame("notice", epoch, message="模型连接已中断，以下为演示知识库原文。")
                        if emitted:
                            yield frame("delta", epoch, text="\n\n" + reply.text)
                if emitted:
                    if active():
                        yield frame("done", epoch)
                    return
            for sentence in re.findall(r"[^。！？]+[。！？]?|[。！？]", reply.text):
                for start in range(0, len(sentence), 10):
                    if not active():
                        return
                    yield frame("delta", epoch, text=sentence[start:start + 10])
                    await asyncio.sleep(0.032)
            if active():
                yield frame("done", epoch)

        return StreamingResponse(events(), media_type="text/event-stream", headers={
            "Cache-Control": "no-cache", "X-Accel-Buffering": "no",
        })

    dist = Path(__file__).resolve().parents[1] / "frontend" / "dist"
    if dist.is_dir():
        api.mount("/", StaticFiles(directory=dist, html=True), name="frontend")
    return api


app = create_app()
