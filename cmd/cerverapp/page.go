//go:build windows

package main

import (
	"os"
	"path/filepath"
	"regexp"
)

var banner = regexp.MustCompile(`Cerver Local Compute\S*\s+v(\d+)`)

// relayVersion: the last "Cerver Local Compute vNNN" the relay printed.
func relayVersion() string {
	f, err := os.Open(filepath.Join(cdir(), "relay.log"))
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, _ := f.Stat()
	off := fi.Size() - 2<<20
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	f.ReadAt(buf, off)
	m := banner.FindAllSubmatch(buf, -1)
	if len(m) == 0 {
		return ""
	}
	return string(m[len(m)-1][1])
}

const pageHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Cerver</title>
<style>
:root{--bg:#f7f7f8;--card:#fff;--ink:#111827;--mute:#6b7280;--line:#e5e7eb;--green:#16a34a;--amber:#d97706;--gray:#94a3b8;--red:#dc2626;--accent:#111827}
@media (prefers-color-scheme:dark){:root{--bg:#0b0d10;--card:#15181d;--ink:#e5e7eb;--mute:#9ca3af;--line:#262a31;--accent:#e5e7eb}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.45 "Segoe UI",system-ui,sans-serif}
main{max-width:860px;margin:0 auto;padding:28px 16px 60px}
header{display:flex;align-items:center;gap:12px;margin-bottom:18px}h1{font-size:22px;margin:0;font-weight:600}
header .sp{flex:1}a{color:inherit}.mute{color:var(--mute)}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:16px 18px;margin-bottom:14px}
.row{display:flex;align-items:center;gap:12px;flex-wrap:wrap}.dot{width:10px;height:10px;border-radius:50%;background:var(--gray)}
.ready .dot{background:var(--green)}.working .dot{background:var(--amber)}
button{font:inherit;padding:6px 12px;border-radius:8px;border:1px solid var(--line);background:var(--card);color:var(--ink);cursor:pointer}
button:hover{border-color:var(--mute)}button:disabled{opacity:.5;cursor:default}
.tiles{display:grid;grid-template-columns:repeat(4,1fr);gap:10px;margin-bottom:14px}
@media (max-width:600px){.tiles{grid-template-columns:repeat(2,1fr)}}
.tile{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:12px 14px}
.tile b{display:block;font-size:22px;font-weight:600}.tile span{color:var(--mute);font-size:12px}
h2{font-size:13px;font-weight:600;color:var(--mute);margin:18px 0 8px;text-transform:none}
.job{display:grid;grid-template-columns:1fr auto;gap:2px 12px;padding:10px 0;border-top:1px solid var(--line)}
.job:first-child{border-top:0}.job .t{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.job .m{color:var(--mute);font-size:12px}.ok{color:var(--green)}.bad{color:var(--red)}.run{color:var(--amber)}
.now{display:flex;gap:24px;flex-wrap:wrap}.now div span{display:block;color:var(--mute);font-size:12px}
pre{white-space:pre-wrap;word-break:break-word;font:12px/1.5 Consolas,monospace;max-height:360px;overflow:auto;margin:10px 0 0}
details summary{cursor:pointer;color:var(--mute)}.empty{color:var(--mute);padding:6px 0}
</style></head><body><main>
<header><h1>Cerver</h1><span class="mute" id="ver"></span><span class="sp"></span><a href="https://cerver.ai/dashboard" target="_blank">Open the dashboard</a></header>

<div class="card" id="relay"><div class="row"><span class="dot"></span><b id="line">Checking the relay…</b>
<span class="mute" id="rv"></span><span class="sp" style="flex:1"></span>
<button id="restart">Restart</button><button id="update">Check for updates</button></div>
<div class="mute" style="margin-top:6px">The relay is what lets your agents, and people you invite, run jobs on this PC. Cerver keeps it running and starts it with Windows.</div>
<pre id="updout" hidden></pre></div>

<div class="card"><div class="mute" style="font-size:12px;margin-bottom:6px">This PC now</div>
<div class="now"><div><span>CPU</span><b id="cpu">–</b></div><div><span>Memory</span><b id="mem">–</b></div><div><span>Free disk</span><b id="disk">–</b></div></div></div>

<div class="tiles"><div class="tile"><b id="tj">–</b><span>Jobs today</span></div><div class="tile"><b id="td">–</b><span>Done</span></div>
<div class="tile"><b id="tf">–</b><span>Failed</span></div><div class="tile"><b id="ts">–</b><span>Time working</span></div></div>

<h2>Working now</h2><div class="card" id="running"><div class="empty">Nothing right now.</div></div>
<h2>Recent jobs</h2><div class="card" id="jobs"><div class="empty">No jobs yet. Jobs sent to this PC show up here.</div></div>
<details class="card"><summary>What the relay prints</summary><pre id="log"></pre></details>
</main><script>
const $=id=>document.getElementById(id);
const esc=s=>String(s??"").replace(/[&<>"]/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));
const gb=b=>b==null?"–":(b/1e9).toFixed(b>=1e11?0:1)+" GB";
const dur=s=>{s=Math.round(s||0);if(s<60)return s+" s";const m=Math.floor(s/60);if(m<60)return m+" min";return Math.floor(m/60)+" h "+(m%60)+" min"};
const ago=t=>{const s=Date.now()/1000-t;if(s<60)return"just now";if(s<3600)return Math.floor(s/60)+" min ago";if(s<86400)return Math.floor(s/3600)+" h ago";return new Date(t*1000).toLocaleDateString()};
async function j(p){const r=await fetch(p);if(!r.ok)throw r.status;return r.json()}
function job(x,running){const st=running?'<span class="run">working · '+dur(Date.now()/1000-x.at)+'</span>':x.status==="done"?'<span class="ok">done · '+dur(x.seconds)+'</span>':'<span class="bad">failed'+(x.seconds?' · '+dur(x.seconds):'')+'</span>';
 const meta=[x.cli,x.from&&("from "+x.from),x.project,ago(x.at)].filter(Boolean).map(esc).join(" · ");
 return '<div class="job"><div class="t">'+esc(x.task||"(no task text)")+'</div><div>'+st+'</div><div class="m">'+meta+(x.error?' · <span class="bad">'+esc(x.error)+'</span>':'')+'</div></div>'}
async function tick(){
 try{const a=await j("/app/state");$("relay").className="card "+a.state;$("line").textContent=a.line;$("ver").textContent="app "+a.version;$("rv").textContent=a.relay?"relay v"+a.relay:""}catch(e){$("line").textContent="Cerver isn't answering"}
 try{const d=await j("/relay/api/cerver/jobs?n=30");$("tj").textContent=d.today.jobs;$("td").textContent=d.today.done;$("tf").textContent=d.today.failed;$("ts").textContent=dur(d.today.seconds);
  $("running").innerHTML=d.running.length?d.running.map(x=>job(x,true)).join(""):'<div class="empty">Nothing right now.</div>';
  $("jobs").innerHTML=d.jobs.length?d.jobs.map(x=>job(x,false)).join(""):'<div class="empty">No jobs yet. Jobs sent to this PC show up here.</div>'}catch(e){}
 try{const s=await j("/relay/api/local-claude/stats");const c=s.compute||{};$("cpu").textContent=c.cpu_percent==null?"–":Math.round(c.cpu_percent)+"%";
  $("mem").textContent=c.memory&&c.memory.percent!=null?Math.round(c.memory.percent)+"% of "+gb(c.memory.total_bytes):"–";$("disk").textContent=c.disk?gb(c.disk.free_bytes):"–"}catch(e){}
 if(document.querySelector("details").open){try{const l=await j("/relay/api/local-claude/logs/tail?n=200");$("log").textContent=(l.lines||[]).map(x=>x.line.replace(/\x1b\[[0-9;]*m/g,"")).join("\n")}catch(e){}}
}
async function post(p){const r=await fetch(p,{method:"POST",headers:{"X-Cerver":"1"}});return r.json()}
$("restart").onclick=async()=>{$("restart").disabled=true;await post("/app/restart");setTimeout(()=>{$("restart").disabled=false;tick()},4000)};
$("update").onclick=async()=>{const b=$("update");b.disabled=true;b.textContent="Checking…";const r=await post("/app/update");$("updout").hidden=false;$("updout").textContent=r.output.trim();b.disabled=false;b.textContent="Check for updates"};
tick();setInterval(tick,3000);if(location.hash==="#update"){history.replaceState(null,"","/");$("update").click()}
</script></body></html>`
