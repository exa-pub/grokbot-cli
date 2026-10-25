// Execute only named pure functions extracted from the local reference bundle.
// The desktop application and its bootstrap/network code are never evaluated.
import fs from 'node:fs';
import vm from 'node:vm';
import path from 'node:path';
const base = process.argv[2] ?? 'ai.tmp/grok-bot/app/dist';
const daemon = fs.readFileSync(path.join(base,'local-exec-daemon/main.cjs'),'utf8');
function extract(name) {
 const start=daemon.indexOf(`function ${name}(`);
 if(start<0) throw Error(`missing ${name}`);
 for(let end=daemon.indexOf('}',start);end>=0;end=daemon.indexOf('}',end+1)) {
  const code=daemon.slice(start,end+1);
  try { new vm.Script(`(${code})`); return code; } catch {}
 }
 throw Error(`cannot parse ${name}`);
}
const context=vm.createContext({Buffer});
for(const name of ['jrt','Sge','yge','xfr','bfr','wce','Ok','Pk']) vm.runInContext(extract(name),context);
vm.runInContext('var bqt=9,Tqt=9;',context);
const cases=[{text:'',args:{}},{text:'one\ntwo\nthree\n',args:{offset:2,limit:1}},{text:'one\ntwo\n',args:{offset:20}},{text:'one\ntwo\nthree',args:{offset:-2}}];
const output={provenance:{version:'0.75.8',platform:'linux',method:'isolated pure functions from source; not desktop traffic',source:'local-exec-daemon/main.cjs',markers:['bfr','Sge','wce','Pk']},ranges:cases.map(c=>({...c,result:context.bfr(c.text,c.text.split('\n').length,c.args)})),discoveryMissing:context.wce('codex'),terminalHeader:context.Pk({pid:42,cwd:'/workspace',command:'printf hello',status:'running',startedAt:'2026-01-01T00:00:00.000Z',runningForMs:0})};
process.stdout.write(JSON.stringify(output,null,2)+'\n');
