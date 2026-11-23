import asyncio
import json
import time

from fastapi.testclient import TestClient
from starlette.requests import Request

from growth.agent import Session
from growth.main import Interrupt, TurnInput, create_app
from growth.model import ChatModel
from growth.store import Evidence, ProfileUpdate, Store


def call(client, lead="harbor"):
    result = client.post("/api/calls", json={"lead_id": lead})
    assert result.status_code == 201
    return result.json()


def turn(client, session, text):
    response = client.post(f"/api/calls/{session}/turns", json={"asr_text": text})
    assert response.status_code == 200
    assert response.headers["content-type"].startswith("text/event-stream")
    events = {}
    for frame in response.text.strip().split("\n\n"):
        header, data = frame.split("\n")
        events.setdefault(header.removeprefix("event: "), []).append(json.loads(data.removeprefix("data: ")))
    return events


def test_registration_quote_booking_idempotency(tmp_path):
    database = tmp_path / "flow.sqlite3"
    with TestClient(create_app(database)) as client:
        session = call(client)["id"]
        assert turn(client, session, "确认认证")["tool"][0]["status"] == "blocked"
        assert turn(client, session, "确认注册 海岚国际货运")["tool"][0]["result"]["stage"] == "registered"
        assert turn(client, session, "确认认证")["tool"][0]["result"]["stage"] == "verified"
        events = turn(client, session, "查价 上海到新加坡")
        assert events["tool"][0]["status"] == "needs_input"
        events = turn(client, session, "40HC 2箱")
        assert events["tool"][0]["result"]["total"] == 1380
        assert len(events["delta"]) > 1 and "done" in events
        assert events["tool"][0]["sources"][0]["ports"] == ["CNSHA", "SGSIN"]
        assert turn(client, session, "确认订舱 WRONG")["tool"][0]["status"] == "needs_input"
        proposal = turn(client, session, "申请订舱")["tool"][0]["result"]["confirmation"]
        first = turn(client, session, proposal)["tool"][0]["result"]["reference"]
        second = turn(client, session, proposal)["tool"][0]["result"]["reference"]
        assert first == second
        with client.app.state.store.connect() as db:
            assert db.execute("SELECT count(*) FROM bookings").fetchone()[0] == 1


def test_profile_is_queued_and_hydrated(tmp_path):
    with TestClient(create_app(tmp_path / "memory.sqlite3")) as client:
        id = call(client)["id"]
        original = "我卡在认证材料没准备好。"
        turn(client, id, original)
        assert client.post(f"/api/calls/{id}/end").json()["profile_update_queued"]
        for _ in range(100):
            profile = client.get("/api/leads").json()[0]["profile"]
            if profile["current_blocker"]:
                break
            time.sleep(0.02)
        resumed = call(client)
        assert resumed["profile"]["evidence"]["current_blocker"]["quote"] == original
        assert profile["current_blocker"] in resumed["greeting"]
        assert turn(client, resumed["id"], "继续处理上次的问题")["tool"][0]["intent"] == "memory"


def test_profile_out_of_order_job_does_not_replace_recent_update(tmp_path):
    store = Store(tmp_path / "ordering.sqlite3")
    old, new = [store.open_call("harbor") for _ in range(2)]
    for seq in (new, old):
        evidence = Evidence(call_seq=seq, utterance_seq=1, quote=str(seq))
        store.enqueue(seq, [ProfileUpdate(field="route_interest", value=str(seq), evidence=evidence)], [])
        assert store.apply_pending() == 1
    _, profile = store.get_lead("harbor")
    assert profile.route_interest == str(new) and profile.revision == 1


def test_opt_out_blocks_next_call(tmp_path):
    with TestClient(create_app(tmp_path / "opt-out.sqlite3")) as client:
        session = call(client, "sail")["id"]
        assert turn(client, session, "请不要再打了")["tool"][0]["intent"] == "opt_out"
        assert client.post("/api/calls", json={"lead_id": "sail"}).status_code == 409
        assert client.post(f"/api/calls/{session}/end").status_code == 200


def test_interrupted_stream_stops_old_deltas(tmp_path):
    with TestClient(create_app(tmp_path / "interrupt.sqlite3")) as client:
        id = call(client)["id"]

        async def run():
            app = client.app
            session: Session = app.state.sessions[id]
            endpoint = next(route.endpoint for route in app.routes if route.name == "turn")
            request = Request({"type": "http", "app": app})
            response = await endpoint(id, TurnInput(asr_text="运价包含哪些费用？"), request)
            stream = response.body_iterator
            assert "event: turn" in await anext(stream)
            assert "event: tool" in await anext(stream)
            assert "event: delta" in await anext(stream)
            interrupt = next(route.endpoint for route in app.routes if route.name == "interrupt")
            result = await interrupt(id, Interrupt(asr_partial_token="等一下"), request)
            assert result["epoch"] == 2 and session.partial == "等一下"
            assert [item async for item in stream] == []

        asyncio.run(run())


def test_optional_model_routes_and_streams_with_retrieval(tmp_path, monkeypatch):
    class MockModel:
        async def choose_read_skill(self, query):
            assert query == "燃油附加费包含在报价里吗？"
            return "knowledge"

        async def stream_knowledge(self, query, evidence):
            assert "[fees]" in evidence and len(evidence) <= 3600
            yield "通常以"
            yield "实际报价单为准。"

    monkeypatch.setattr(ChatModel, "from_env", classmethod(lambda cls: MockModel()))
    with TestClient(create_app(tmp_path / "model.sqlite3")) as client:
        session = call(client)["id"]
        events = turn(client, session, "燃油附加费包含在报价里吗？")
        assert [item["text"] for item in events["delta"]] == ["通常以", "实际报价单为准。"]
        assert events["tool"][0]["intent"] == "knowledge"
