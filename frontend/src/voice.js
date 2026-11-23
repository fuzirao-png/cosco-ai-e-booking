export const SpeechRecognition = window.SpeechRecognition || window.webkitSpeechRecognition;
let tts_text_queue = '';

export function stopSpeech() {
  tts_text_queue = '';
  window.speechSynthesis?.cancel();
}

export function speakDelta(delta, flush = false) {
  if (!window.speechSynthesis) return;
  tts_text_queue += delta;
  let boundary = tts_text_queue.search(/[。！？；\n]/);
  while (boundary !== -1 || tts_text_queue.length > 65) {
    const end = boundary >= 0 && boundary < 65 ? boundary + 1 : 65;
    speak(tts_text_queue.slice(0, end));
    tts_text_queue = tts_text_queue.slice(end);
    boundary = tts_text_queue.search(/[。！？；\n]/);
  }
  if (flush && tts_text_queue.trim()) {
    speak(tts_text_queue);
    tts_text_queue = '';
  }
}

function speak(text) {
  const utterance = new SpeechSynthesisUtterance(text);
  utterance.lang = 'zh-CN';
  utterance.rate = 1;
  const voice = window.speechSynthesis.getVoices().find(item => /^zh(-|_)/i.test(item.lang));
  if (voice) utterance.voice = voice;
  window.speechSynthesis.speak(utterance);
}
