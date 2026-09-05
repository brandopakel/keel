import tcp from 'k6/x/tcp';
import { Counter, Rate, Trend } from 'k6/metrics';
import { encode, parse } from './resp.mjs';

const requests = new Counter('keel_requests');
const connectionErrors = new Counter('keel_connection_errors');
const errors = new Rate('keel_errors');
const latency = new Trend('keel_latency', true);
const rate = Number(__ENV.RATE || 10);
const batch = Number(__ENV.BATCH || 20);
if (!Number.isInteger(rate) || rate<1 || !Number.isInteger(batch) || batch<1 || batch>1000) throw new Error('invalid RATE/BATCH');
export const options = {
  scenarios: { cache: {executor:'constant-arrival-rate',rate,timeUnit:'1s',duration:__ENV.DURATION||'30s',preAllocatedVUs:Number(__ENV.VUS||10),maxVUs:Number(__ENV.MAX_VUS||100)} },
  thresholds: {keel_connection_errors:['count==0'],keel_errors:['rate==0'],dropped_iterations:['count==0'],keel_latency:[`p(99)<${Number(__ENV.P99_MS||100)}`]},
};
export function setup() { return {prefix:__ENV.RUN_ID || `k6-${Date.now()}`}; }

class Client {
  constructor() {
    this.socket=new tcp.Socket(); this.bytes=new Uint8Array(0); this.pending=null;
    const fail=err=>{if(this.pending){const p=this.pending;this.pending=null;p.reject(err);} this.socket.destroy();};
    this.socket.on('error',fail);
    this.socket.on('timeout',()=>fail(new Error('socket timeout')));
    this.socket.on('close',()=>{if(this.pending)fail(new Error('socket closed'));});
    this.socket.on('data',chunk=>{
      try {
        const incoming=new Uint8Array(chunk);
        if(this.bytes.length+incoming.length>16*1024*1024)throw new Error('reply buffer limit');
        const joined=new Uint8Array(this.bytes.length+incoming.length);joined.set(this.bytes);joined.set(incoming,this.bytes.length);this.bytes=joined;
        const result=parse(this.bytes);
        if(result && this.pending){const p=this.pending;this.pending=null;this.bytes=this.bytes.slice(result.next);result.error?p.reject(new Error(result.value)):p.resolve(result.value);}
      } catch(err){fail(err);}
    });
  }
  async connect(){ await this.socket.connect({host:__ENV.KEEL_HOST||'127.0.0.1',port:Number(__ENV.KEEL_PORT||6379),tls:__ENV.KEEL_TLS==='1'});this.socket.setTimeout(3000);if(__ENV.KEEL_PASSWORD)await this.call(['AUTH',__ENV.KEEL_PASSWORD]); }
  call(parts) {
    return new Promise((resolve,reject)=>{
      this.pending={resolve,reject};
      this.socket.write(encode(parts)).catch(err=>{this.pending=null;reject(err);this.socket.destroy();});
    });
  }
}
export default async function(data) {
  const client=new Client();let connected=false;
  try {
    await client.connect();connected=true;connectionErrors.add(0);
    const key=`${data.prefix}:${__VU}:${__ITER}`;
    for(let i=0;i<batch;i++) {
      const parts=i%2===0?['SET',key,'v'.repeat(256),'PX','60000']:['GET',key];
      const start=Date.now();let failed=false;
      try {
        const value=await client.call(parts);
        if(parts[0]==='SET' && value!=='OK')throw new Error('SET mismatch');
        if(parts[0]==='GET' && (!(value instanceof Uint8Array)||value.length!==256||value.some(b=>b!==118)))throw new Error('GET mismatch');
      } catch(err){failed=true;throw err;}
      finally {requests.add(1,{command:parts[0]});errors.add(failed);latency.add(Date.now()-start,{command:parts[0]});}
    }
  } catch(err) {if(!connected)connectionErrors.add(1);throw err;}
  finally {client.socket.destroy();}
}
