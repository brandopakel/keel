// Byte-oriented RESP2 framing: TCP reads may split or combine any frames.
export function encode(parts) {
  const bytes = [];
  const add = s => { for (const c of unescape(encodeURIComponent(String(s)))) bytes.push(c.charCodeAt(0)); };
  add(`*${parts.length}\r\n`);
  for (const part of parts) {
    const raw = unescape(encodeURIComponent(String(part)));
    add(`$${raw.length}\r\n`);
    for (let i=0;i<raw.length;i++) bytes.push(raw.charCodeAt(i));
    add('\r\n');
  }
  return new Uint8Array(bytes).buffer;
}
export function parse(bytes, offset=0, depth=0) {
  if (depth>16) throw new Error('RESP nesting limit');
  let end=offset;
  while (end+1<bytes.length && !(bytes[end]===13 && bytes[end+1]===10)) end++;
  if (end+1>=bytes.length) return null;
  const kind=bytes[offset];
  let line=''; for(let i=offset+1;i<end;i++) line+=String.fromCharCode(bytes[i]);
  let next=end+2;
  if (kind===43 || kind===45) return {value:line,error:kind===45,next};
  if (!/^-?\d+$/.test(line)) throw new Error('invalid RESP number');
  const n=Number(line);
  if (!Number.isSafeInteger(n)) throw new Error('RESP integer overflow');
  if(kind===58) return {value:n,next};
  if(n===-1 && (kind===36 || kind===42)) return {value:null,next};
  if(n<0 || n>16*1024*1024) throw new Error('RESP length limit');
  if(kind===36) {
    if(bytes.length<next+n+2) return null;
    if(bytes[next+n]!==13 || bytes[next+n+1]!==10) throw new Error('invalid bulk terminator');
    return {value:bytes.slice(next,next+n),next:next+n+2};
  }
  if(kind===42) {
    const value=[];
    for(let i=0;i<n;i++) { const item=parse(bytes,next,depth+1); if(!item)return null; if(item.error)throw new Error(item.value); value.push(item.value); next=item.next; }
    return {value,next};
  }
  throw new Error('unknown RESP type');
}
