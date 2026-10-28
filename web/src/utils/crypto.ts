// 登录密码信封加密：WebCrypto RSA-OAEP(SHA-256)。
// 服务端提供 SPKI PEM 公钥；密文载荷为 JSON {"p":密码,"ts":Unix秒}，
// 服务端据此拒绝超时重放。浏览器非安全上下文（http 且非 localhost）没有
// crypto.subtle，此时返回空串，由调用方回退明文（依赖 HTTPS 传输）。

/** PEM → ArrayBuffer（SPKI DER） */
function pemToDer(pem: string): ArrayBuffer {
  const b64 = pem.replace(/-----[^-]+-----/g, '').replace(/\s+/g, '')
  const bin = atob(b64)
  const buf = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i)
  return buf.buffer
}

/** 加密密码；不可用（非安全上下文）或加密失败时返回 '' */
export async function encryptPassword(password: string, publicKeyPEM: string): Promise<string> {
  if (!window.crypto?.subtle) return ''
  try {
    const key = await crypto.subtle.importKey(
      'spki',
      pemToDer(publicKeyPEM),
      { name: 'RSA-OAEP', hash: 'SHA-256' },
      false,
      ['encrypt'],
    )
    const payload = new TextEncoder().encode(JSON.stringify({
      p: password,
      ts: Math.floor(Date.now() / 1000),
    }))
    const enc = await crypto.subtle.encrypt({ name: 'RSA-OAEP' }, key, payload)
    const bytes = new Uint8Array(enc)
    let bin = ''
    for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i])
    return btoa(bin)
  } catch (e) {
    console.warn('[u-file] 登录密码加密失败，将回退明文传输（建议启用 HTTPS）', e)
    return ''
  }
}
