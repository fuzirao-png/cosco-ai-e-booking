import React, { useEffect, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import {
  Activity, Anchor, ArrowRight, AudioLines, BadgeCheck, Building2, ChevronRight, CircleHelp,
  Container, Database, Headphones, Mic, MicOff, Phone, PhoneOff, Play, Send, Square,
  Volume2, VolumeX,
} from 'lucide-react';

import { api, readEvents } from './stream';
import { SpeechRecognition, speakDelta, stopSpeech } from './voice';
import './style.css';

const stages = { new: '待注册', registered: '待认证', verified: '已认证' };
const stageSteps = ['注册', '认证', '查价', '订舱'];
const labels = {
  quote: '查询运价', status: '企业状态', register: '企业注册', verify: '企业认证',
  book: '订舱预览', confirm: '确认订舱', knowledge: '知识库检索', memory: '画像续接',
  opt_out: '停止联系',
};
const outcome = {
  ok: '完成', needs_input: '待补充', blocked: '需处理', not_found: '无匹配',
  guarded: '已拦截', preview: '待确认',
};
const wave = [6, 14, 10, 23, 42, 20, 32, 48, 25, 37, 53, 39, 20, 45, 65, 43, 24,
  52, 40, 26, 56, 75, 45, 63, 92, 52, 68, 94, 64, 46, 72, 98, 69, 49, 78, 58, 30,
  60, 76, 48, 29, 59, 45, 27, 37, 52, 34, 18, 44, 31, 17, 29, 19, 10, 15, 6];

function App() {
  const [leads, setLeads] = useState([]);
  const [selected, setSelected] = useState('harbor');
  const [call, setCall] = useState(null);
  const [messages, setMessages] = useState([]);
  const [tools, setTools] = useState([]);
  const [draft, setDraft] = useState('');
  const [partial, setPartial] = useState('');
  const [working, setWorking] = useState(false);
  const [streaming, setStreaming] = useState(false);
  const [listening, setListening] = useState(false);
  const [speech, setSpeech] = useState(false);
  const [error, setError] = useState('');
  const [tab, setTab] = useState('tools');
  const [health, setHealth] = useState(null);
  const [now, setNow] = useState(Date.now());
  const activeStream = useRef(null);
  const interruptRequest = useRef(Promise.resolve());
  const recognition = useRef(null);
  const messageArea = useRef(null);
  const speechEnabled = useRef(false);
  const voiceCallbacks = useRef({});
  const command = useRef(null);

  const lead = leads.find(item => item.id === selected);
  const recentQuote = tools.find(item => item.intent === 'quote');
  const recentPreview = tools.find(item => item.intent === 'book');
  const booking = tools.find(item => item.result.reference);
  const lastStop = tools.find(item => item.intent === 'opt_out');
  const sources = [...new Map(tools.flatMap(item => item.sources).map(item => [item.id, item])).values()];
  const confirm = recentPreview && recentPreview === tools.find(item => ['quote', 'book'].includes(item.intent))
    ? recentPreview.result.confirmation : undefined;
  const elapsed = call ? Math.max(0, Math.floor((now - call.started) / 1000)) : 0;
  const clock = [Math.floor(elapsed / 60), elapsed % 60].map(n => String(n).padStart(2, '0')).join(':');

  async function refreshLeads() {
    setLeads(await (await api('/leads')).json());
  }

  useEffect(() => {
    refreshLeads().catch(err => setError(err.message));
    api('/health').then(response => response.json()).then(setHealth).catch(() => {});
  }, []);

  useEffect(() => {
    if (!call) return undefined;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [call]);

  useEffect(() => {
    messageArea.current?.scrollTo({ top: messageArea.current.scrollHeight });
  }, [messages, partial]);

  async function startCall() {
    if (!lead || working) return;
    setWorking(true);
    setError('');
    try {
      const data = await (await api('/calls', { body: { lead_id: selected } })).json();
      setCall({ ...data, started: Date.now() });
      setMessages([{ id: 'greeting', role: 'agent', text: data.greeting }]);
      setTools([]);
      setTab('tools');
      if (speechEnabled.current) speakDelta(data.greeting, true);
      requestAnimationFrame(() => command.current?.focus());
    } catch (err) {
      setError(err.message);
    } finally {
      setWorking(false);
    }
  }

  function stopStream() {
    if (activeStream.current) {
      const { controller, id } = activeStream.current;
      activeStream.current = null;
      controller.abort();
      setMessages(items => items.map(item => item.id === id ? { ...item, stopped: true } : item));
    }
    setStreaming(false);
    stopSpeech();
  }

  function interrupt(text = '') {
    if (!call) return;
    stopStream();
    interruptRequest.current = interruptRequest.current.then(() =>
      api('/calls/' + call.id + '/interrupt', { body: { asr_partial_token: text } })
    ).catch(err => setError(err.message));
  }

  function stopMic() {
    const current = recognition.current;
    recognition.current = null;
    current?.abort();
    setListening(false);
    setPartial('');
  }

  async function endCall() {
    if (!call || working) return;
    setWorking(true);
    setError('');
    stopMic();
    stopStream();
    try {
      await interruptRequest.current;
      await api('/calls/' + call.id + '/end', { method: 'POST' });
      setCall(null);
      await refreshLeads();
      setTimeout(() => refreshLeads().catch(() => {}), 700);
    } catch (err) {
      setError(err.message);
    } finally {
      setWorking(false);
    }
  }

  async function submit(text) {
    text = text.trim();
    if (!call || !text || working || lastStop) return;
    stopStream();
    setDraft('');
    setPartial('');
    setError('');
    const id = crypto.randomUUID();
    const controller = new AbortController();
    activeStream.current = { controller, id };
    setStreaming(true);
    setMessages(items => [...items, { id: id + 'customer', role: 'customer', text },
      { id, role: 'agent', text: '' }]);
    try {
      await interruptRequest.current;
      const response = await api('/calls/' + call.id + '/turns', {
        body: { asr_text: text }, signal: controller.signal,
      });
      let epoch;
      let completed = false;
      await readEvents(response, (type, event) => {
        if (activeStream.current?.id !== id) return;
        if (type === 'turn') epoch = event.epoch;
        if (event.epoch !== epoch) return;
        if (type === 'delta') {
          setMessages(items => items.map(item => item.id === id
            ? { ...item, text: item.text + event.text } : item));
          if (speechEnabled.current) speakDelta(event.text);
        }
        if (type === 'tool') {
          setTools(items => [{ ...event, id }, ...items]);
          if (event.result.stage) {
            setLeads(items => items.map(item => item.id === selected
              ? { ...item, stage: event.result.stage } : item));
          }
          if (event.intent === 'opt_out') {
            stopMic();
            refreshLeads().catch(() => {});
          }
        }
        if (type === 'notice') setError(event.message);
        if (type === 'done') {
          completed = true;
          if (speechEnabled.current) speakDelta('', true);
        }
      });
      if (!completed && activeStream.current?.id === id) stopStream();
    } catch (err) {
      if (err.name !== 'AbortError' && activeStream.current?.id === id) {
        setError(err.message);
        stopStream();
      }
    } finally {
      if (activeStream.current?.id === id) {
        activeStream.current = null;
        setStreaming(false);
      }
    }
  }

  voiceCallbacks.current = { submit, interrupt };

  function toggleMic() {
    if (listening) return stopMic();
    if (!SpeechRecognition) {
      setError('当前浏览器不支持语音识别。可以在下方输入文字。');
      return;
    }
    const mic = new SpeechRecognition();
    recognition.current = mic;
    mic.lang = 'zh-CN';
    mic.interimResults = true;
    mic.continuous = true;
    let interrupted = false;
    mic.onspeechstart = () => {
      if (recognition.current !== mic) return;
      interrupted = false;
      if (activeStream.current || window.speechSynthesis?.speaking) {
        voiceCallbacks.current.interrupt();
        interrupted = true;
      }
    };
    mic.onresult = event => {
      if (recognition.current !== mic) return;
      let final = '';
      let interim = '';
      for (let index = event.resultIndex; index < event.results.length; index++) {
        const result = event.results[index];
        if (result.isFinal) final += result[0].transcript;
        else interim += result[0].transcript;
      }
      if (interim && (interrupted || activeStream.current || window.speechSynthesis?.speaking)) {
        voiceCallbacks.current.interrupt(interim);
        interrupted = false;
      }
      setPartial(interim);
      if (final.trim()) voiceCallbacks.current.submit(final);
    };
    mic.onerror = event => {
      if (recognition.current === mic && event.error !== 'aborted') {
        setError('语音识别暂不可用（' + event.error + '），可继续使用文字输入。');
      }
    };
    mic.onend = () => {
      if (recognition.current === mic) stopMic();
    };
    try {
      mic.start();
      setListening(true);
    } catch {
      stopMic();
      setError('无法开启麦克风。');
    }
  }

  function toggleSpeech() {
    speechEnabled.current = !speechEnabled.current;
    setSpeech(speechEnabled.current);
    if (!speechEnabled.current) stopSpeech();
  }

  const prompts = [
    lead?.stage === 'new' && { title: '企业注册', text: '确认注册 ' + lead.company },
    lead?.stage === 'registered' && { title: '提交认证', text: '确认认证' },
    lead?.profile.current_blocker && { title: '续接上次卡点', text: '继续处理上次的问题' },
    { title: '查询运价', text: selected === 'meridian' ? '查询宁波到洛杉矶 40HC 2箱运价' : '查询上海到新加坡 40HC 2箱运价' },
    confirm && !booking
      ? { title: '确认订舱', text: confirm }
      : { title: '申请订舱', text: '申请订舱' },
    { title: '费用说明', text: '运价包含哪些费用？' },
  ].filter(Boolean);

  return (
    <div className="shell">
      <header className="top">
        <div className="brand"><span className="brand-icon"><Anchor size={22} /></span>
          <div><strong>E-BOOKING</strong><small>GROWTH AGENT</small></div></div>
        <span className="top-divider" />
        <span className="top-caption">外呼工作台</span>
        <div className="top-status"><span className={health ? 'dot online' : 'dot'} />
          {health ? (health.model === 'local' ? '本地演示' : '模型模式') : '连接服务中'}</div>
      </header>

      <main className="workspace">
        <div className="title"><div><h1>外呼工作台</h1><p>从线索跟进到线上订舱</p></div>
          <div className="lab"><Headphones size={15} /> 模拟通话</div></div>

        <div className="layout">
          <aside className="panel leads">
            <div className="section-head"><Building2 size={17} /><h2>客户线索</h2><span>{leads.length}</span></div>
            <div className="lead-list">
              {leads.map(item => <button key={item.id} className={'lead ' + (selected === item.id ? 'selected' : '')}
                disabled={Boolean(call) || working} onClick={() => { setSelected(item.id); setMessages([]); setTools([]); setError(''); }}>
                <span className="avatar">{item.company.slice(0, 1)}</span>
                <span className="lead-name"><strong>{item.company}</strong><small>{item.contact} <span>·</span> {item.opted_out ? '停止联系' : stages[item.stage]}</small></span>
                <ChevronRight size={14} />
              </button>)}
            </div>
            {lead && <div className="lead-note"><h3>线索备注</h3><p>{lead.note}</p></div>}
          </aside>

          <section className="panel conversation" aria-label="模拟通话">
            <div className="call-head">
              <div className="call-id"><div className="call-icon"><AudioLines size={20} /></div>
                <div><h2>{lead?.company ?? '外呼会话'}</h2><p><span className={call ? 'dot online' : 'dot'} />
                  {call ? '通话中' : '等待呼叫'} <span className="clock">{clock}</span></p></div></div>
              <button className={call ? 'button danger' : 'button primary'} disabled={working || !lead || (!call && lead.opted_out)}
                onClick={call ? endCall : startCall}>{call ? <PhoneOff size={16} /> : <Phone size={16} />}
                {call ? '结束通话' : '开始通话'}</button>
            </div>
            <div className={'waveform ' + (listening || streaming ? 'running' : '')} aria-hidden="true">
              <span className="wave-caption"><Activity size={12} /> {listening ? '麦克风已开启' : streaming ? '流式响应' : '语音通道'}</span>
              <div className="wave-bars">{wave.map((bar, index) => <i key={index}
                style={{ '--level': bar + '%', '--offset': (index % 13) * -53 + 'ms' }} />)}</div>
            </div>
            <div className="transcript" ref={messageArea} aria-label="对话记录">
              {!messages.length && <div className="empty conversation-empty">
                <span className="empty-icon"><Headphones size={25} /></span><strong>准备开始通话</strong>
                <p>呼叫选中的客户，使用文字或麦克风进行模拟对话。</p>
              </div>}
              {messages.map((item, index) => <div key={item.id} className={'message ' + item.role}>
                <span className="message-who">{item.role === 'agent' ? 'Growth Agent' : lead?.contact}</span>
                <div className="bubble">{item.text || (item.stopped ? '回复已停止' : '正在响应…')}
                  {item.text && item.stopped && <small>回复已停止</small>}
                  {streaming && index === messages.length - 1 && <span className="cursor" />}</div>
              </div>)}
              {partial && <p className="partial">正在识别 · {partial}</p>}
            </div>
            <div className="composer">
              {error && <div className="error" role="alert">{error}</div>}
              {(call && !lastStop) && <div className="prompts">{prompts.map(item => <button key={item.title}
                onClick={() => submit(item.text)} disabled={working || streaming}><Play size={11} />{item.title}</button>)}</div>}
              <form className="input-row" onSubmit={event => { event.preventDefault(); submit(draft); }}>
                <button type="button" className={'icon-button ' + (listening ? 'enabled' : '')} disabled={!call || working || Boolean(lastStop)}
                  onClick={toggleMic} title={listening ? '关闭麦克风' : '开启麦克风'}
                  aria-label={listening ? '关闭麦克风' : '开启麦克风'}>{listening ? <Mic size={18} /> : <MicOff size={18} />}</button>
                <button type="button" className={'icon-button ' + (speech ? 'enabled' : '')} onClick={toggleSpeech}
                  title={speech ? '关闭播报' : '开启播报'} aria-label={speech ? '关闭播报' : '开启播报'}>
                  {speech ? <Volume2 size={18} /> : <VolumeX size={18} />}</button>
                <input ref={command} value={draft} maxLength={1200} onChange={event => setDraft(event.target.value)}
                  disabled={!call || working || Boolean(lastStop)} aria-label="客户消息"
                  placeholder={call ? '输入客户的话…' : '开始通话后输入消息'} />
                {streaming && <button type="button" className="icon-button stop" onClick={() => interrupt()} aria-label="打断回复"><Square size={15} /></button>}
                <button type="submit" className="send" disabled={!call || working || !draft.trim() || Boolean(lastStop)} aria-label="发送">
                  <Send size={17} /></button>
              </form>
            </div>
          </section>

          <aside className="right">
            <section className="panel journey">
              <div className="section-head"><Container size={18} /><h2>订舱进度</h2></div>
              <div className="steps">{stageSteps.map((label, index) => {
                const passed = [lead?.stage !== 'new' && Boolean(lead), lead?.stage === 'verified',
                  recentQuote?.status === 'ok', Boolean(booking)];
                return <div className={'step ' + (passed[index] ? 'complete' : '')} key={label}>
                  <span>{passed[index] ? <BadgeCheck size={16} /> : index + 1}</span><small>{label}</small>
                </div>;
              })}</div>
              {recentQuote?.result.total && <div className="quote-card">
                <span>{recentQuote.result.route}</span><strong>USD {recentQuote.result.total.toLocaleString('en-US')}</strong>
                <small>{recentQuote.result.request.container_type} × {recentQuote.result.request.cargo_volume} · 样例海运费</small>
              </div>}
              {booking && <div className="booking"><BadgeCheck size={15} />{booking.result.reference}</div>}
            </section>

            <section className="panel trace">
              <div className="section-head"><Activity size={17} /><h2>会话详情</h2></div>
              <div className="tabs" aria-label="会话详情">
                {[['tools', '执行记录'], ['sources', '知识引用'], ['memory', '客户画像']].map(([id, name]) =>
                  <button key={id} className={tab === id ? 'current' : ''} onClick={() => setTab(id)}>{name}</button>)}
              </div>
              <div className="tab-content">
                {tab === 'tools' && (tools.length ? <ol className="logs">{tools.map(item => <li key={item.id}>
                  <span className="log-icon"><ArrowRight size={13} /></span><span>{labels[item.intent] ?? item.intent}<small>轮次 {item.epoch}</small></span>
                  <em>{outcome[item.status] ?? item.status}</em></li>)}</ol> : <div className="empty compact"><Activity size={20} /><p>工具执行记录会显示在这里</p></div>)}
                {tab === 'sources' && (sources.length ? <ol className="logs">{sources.map(item => <li key={item.id}>
                  <span className="log-icon"><Database size={13} /></span><span>{item.title}<small>{item.id}</small></span>
                </li>)}</ol> : <div className="empty compact"><Database size={20} /><p>本次通话暂无知识引用</p></div>)}
                {tab === 'memory' && (lead ? <div className="memory">
                  <span>画像版本 {lead.profile.schema_version} · 修订 {lead.profile.revision}</span>
                  <dl><dt>当前卡点</dt><dd>{lead.profile.current_blocker ?? '暂无'}</dd>
                    <dt>关注航线</dt><dd>{lead.profile.route_interest ?? '暂无'}</dd></dl>
                  {Object.entries(lead.profile.evidence).map(([field, value]) =>
                    <blockquote key={field}>{value.quote}<small>会话 {value.call_seq} · 发言 {value.utterance_seq}</small></blockquote>)}
                </div> : <div className="empty compact"><Database size={20} /><p>请选择客户线索</p></div>)}
              </div>
            </section>
          </aside>
        </div>
        <footer><CircleHelp size={14} /> 演示线索、运价及订舱记录均为合成数据。</footer>
      </main>
    </div>
  );
}

createRoot(document.getElementById('root')).render(<App />);
