export async function api(path, { body, ...options } = {}) {
  const response = await fetch('/api' + path, {
    ...options,
    ...(body === undefined ? {} : {
      method: 'POST',
      body: JSON.stringify(body),
      headers: { 'Content-Type': 'application/json' },
    }),
  });
  if (!response.ok) {
    const problem = await response.json().catch(() => ({}));
    throw new Error(typeof problem.detail === 'string' ? problem.detail : '服务请求失败');
  }
  return response;
}

export async function readEvents(response, onEvent) {
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = '';
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += value;
      let separator;
      while ((separator = buffer.match(/\r?\n\r?\n/))) {
        const at = separator.index;
        const frame = buffer.slice(0, at);
        buffer = buffer.slice(at + separator[0].length);
        const lines = frame.split(/\r?\n/);
        const type = lines.find(line => line.startsWith('event:'))?.slice(6).trim();
        const data = lines.filter(line => line.startsWith('data:'))
          .map(line => line.slice(5).replace(/^ /, '')).join('\n');
        if (type && data) onEvent(type, JSON.parse(data));
      }
    }
  } finally {
    reader.releaseLock();
  }
}
