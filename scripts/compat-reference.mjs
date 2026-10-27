// Execute only named pure functions extracted from the local reference bundle.
// The desktop application and its bootstrap/network code are never evaluated;
// the streaming reader gets its file chunks from an in-memory stub.
import fs from 'node:fs';
import vm from 'node:vm';
import path from 'node:path';
import util from 'node:util';
import {isUtf8} from 'node:buffer';
const base = process.argv[2] ?? 'ai.tmp/grok-bot/0.77.0/app/dist';
const daemon = fs.readFileSync(path.join(base,'local-exec-daemon/main.cjs'),'utf8');
function extract(name) {
 const forms=[`async function*${name}(`,`async function ${name}(`,`function ${name}(`,`var ${name}=class`];
 const form=forms.find(f=>daemon.includes(f));
 if(form===undefined) throw Error(`missing ${name}`);
 const start=daemon.indexOf(form);
 for(let end=daemon.indexOf('}',start);end>=0;end=daemon.indexOf('}',end+1)) {
  const code=daemon.slice(start,end+1);
  try { new vm.Script(form.startsWith('var ')?code:`(${code})`); return code; } catch {}
 }
 throw Error(`cannot parse ${name}`);
}
// Stand-ins for the node modules the streaming reader imports: the file is
// read from memory in 64 KiB chunks, as createReadStream would.
const chunked=(text,size)=>({async *[Symbol.asyncIterator](){const b=Buffer.from(text);for(let i=0;i<b.length;i+=size)yield b.subarray(i,i+size)}});
const context=vm.createContext({Buffer,Math,T_e:{TextDecoder:util.TextDecoder},y_e:{isUtf8},E_e:{createReadStream:(text,o)=>chunked(text,o.highWaterMark)}});
for(const name of ['Aat','v_e','R_e','$2r','J2r','ude','jO','GO','Iat','__e','og','Fat','Uat','b_e','h9t','g9t','ORe','S9t','NRe','Ute','x_e','Cat','Y2r','S_e','e3r','t3r','Pat']) vm.runInContext(extract(name),context);
vm.runInContext('var XKt=9,QKt=9,w_e=64*1024;',context);
// readTextStreaming as the Linux read executor runs it, file path = text.
context.stream=async(text,args)=>{const r=await context.Pat({signal:undefined},text,args,{kind:'strictUtf8',cacheEncoding:'utf8'},false,Buffer.byteLength(text));return {...r,content:context.__e(r.content,r.truncated,args)}};
const cases=[{text:'',args:{}},{text:'one\ntwo\nthree\n',args:{offset:2,limit:1}},{text:'one\ntwo\n',args:{offset:20}},{text:'one\ntwo\nthree',args:{offset:-2}}];
const streamCases=[...cases,
 {text:'one\ntwo\nthree\n',args:{maxChars:5}},
 {text:'one\ntwo\nthree\n',args:{maxChars:7}},
 {text:'one\ntwo\nthree',args:{maxChars:100}},
 {text:'one\ntwo\nthree\nfour\n',args:{offset:2,maxChars:4}},
 {text:'one\ntwo\nthree\nfour\n',args:{offset:2,limit:1,maxChars:1}},
 {text:'one\ntwo\nthree\nfour',args:{offset:-2,maxChars:1}},
 {text:'one\ntwo\n',args:{offset:20,maxChars:1}},
 {text:'ab\r\ncd\r\nef\r\n',args:{maxChars:5}},
 {text:'ab\r\ncd\r\nef\r\n',args:{maxChars:4}},
 {text:'\u{1F600}\u{1F600}\nx',args:{maxChars:1}},
 {text:'a'.repeat(70000)+'\nb',args:{maxChars:10}},
 {text:'a'+'é'.repeat(40000),args:{maxChars:10}},
 {text:'﻿'+'a'.repeat(70000),args:{maxChars:10}},
 {text:'x\n'+'a'.repeat(140000)+'\ny',args:{offset:2,maxChars:70000}},
];
// How the reader decodes a file that is not valid UTF-8: lossy UTF-8 if W3
// finds it mostly UTF-8, Latin-1 otherwise (Jat).
vm.runInContext(extract('W3'),context);
vm.runInContext('var Pze=2;',context);
const fallback=['636166e920617520616974','d0bfd180d0b8d0b2d0b5d18220ff20d0bcd0b8d180','e28220616263','e697a5e69cace8aa9eff fe e697a5'.replace(/ /g,''),'61ff fe62'.replace(/ /g,'')].map(hex=>{const b=Buffer.from(hex,'hex'),lossy=new util.TextDecoder('utf-8').decode(b);return {hex,mostlyUtf8:context.W3(lossy),lossy}});
// Long texts are stored as runs of one character to keep the file small.
const runs=t=>[...t.matchAll(/(.)\1*/gsu)].map(m=>({char:m[1],count:[...m[0]].length}));
const streaming=[];
for(const c of streamCases){
 const {content,...r}=await context.stream(c.text,c.args);
 streaming.push(c.text.length>200?{textRuns:runs(c.text),args:c.args,result:{contentRuns:runs(content),...r}}:{text:c.text,args:c.args,result:{content,...r}});
}
const output={provenance:{version:'0.77.0',platform:'linux',method:'isolated pure functions from source; not desktop traffic',source:'local-exec-daemon/main.cjs',markers:['J2r','v_e','ude','GO','Pat','__e']},ranges:cases.map(c=>({...c,result:context.J2r(c.text,c.text.split('\n').length,c.args)})),streaming,fallback,discoveryMissing:context.ude('codex'),terminalHeader:context.GO({pid:42,cwd:'/workspace',command:'printf hello',status:'running',startedAt:'2026-01-01T00:00:00.000Z',runningForMs:0})};
process.stdout.write(JSON.stringify(output,null,2)+'\n');
