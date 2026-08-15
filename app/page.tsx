"use client";

import { FormEvent, useCallback, useEffect, useRef, useState } from "react";

type NodeRole = "ingress" | "egress" | "both";
type Mode = "dual_managed" | "exit_only";
type Engine = "nftables" | "realm";
type Node = { id:string; name:string; role:NodeRole; public_address:string; private_address:string; public_interface:string; private_interface:string; status:string; agent_version?:string; applied_revision?:number; apply_status?:string; apply_error?:string; last_seen_at:string };
type Line = { id:string; name:string; mode:Mode; ingress_node_id:string; egress_node_id:string; egress_node_ids:string[]; active_egress_node_id:string; failover_enabled:boolean; listen_address:string; relay_port_range:string; engine:Engine; ingress_engine:Engine; egress_engine:Engine; enabled:boolean };
type Rule = { id:string; line_id:string; mode:Mode; name:string; protocol:"tcp"|"udp"|"both"; ingress_node_id:string; egress_node_id:string; listen_address:string; listen_port:number; relay_port:number; target_host:string; target_port:number; engine:Engine; ingress_engine:Engine; egress_engine:Engine; upload_mbps:number; download_mbps:number; burst_kbytes:number; enabled:boolean };
type Point = { bucket:string; upload_bytes:number; download_bytes:number };
type RuleTraffic = { rule_id:string; total_upload_bytes:number; total_download_bytes:number; today_upload_bytes:number; today_download_bytes:number; week_upload_bytes:number; week_download_bytes:number; month_upload_bytes:number; month_download_bytes:number; quarter_upload_bytes:number; quarter_download_bytes:number; upload_bytes_per_second:number; download_bytes_per_second:number };
type Summary = { online_nodes:number; total_nodes:number; enabled_rules:number; total_rules:number; today_upload:number; today_download:number; week_upload:number; week_download:number; month_upload:number; month_download:number; quarter_upload:number; quarter_download:number; recent_traffic:Point[] };
type View = "overview"|"nodes"|"lines"|"rules"|"traffic"|"settings";
type TrafficPeriod = "day"|"week"|"month"|"quarter";
type ChartMode = "line"|"bar";
type ThemeMode = "light"|"dark"|"system";
type LinkProbe = { ingress_node_id:string; egress_node_id:string; address:string; latency_ms:number; packet_loss:number; success:boolean; has_succeeded:boolean; failure_count:number; success_count:number; checked_at:string };
type TargetProbe = { rule_id:string; node_id:string; address:string; port:number; latency_ms:number; packet_loss:number; success:boolean; has_succeeded:boolean; failure_count:number; success_count:number; tcp_checked:boolean; tcp_success:boolean; tcp_latency_ms:number; tcp_error?:string; checked_at:string };
type ConfigBackup = { format:string; schema_version:number; exported_at:string; required_nodes:Array<Pick<Node,"id"|"name"|"role">>; lines:Line[]; rules:Rule[] };
type ConfigImportResult = { dry_run:boolean; lines:number; rules:number; revision?:number };

const demoNodes:Node[] = [
  {id:"in-gz",name:"广州入口",role:"ingress",public_address:"203.0.113.18",private_address:"10.24.0.2",public_interface:"eth0",private_interface:"wg0",status:"online",agent_version:"0.3.6",apply_status:"normal",last_seen_at:new Date().toISOString()},
  {id:"out-hkg",name:"香港出口",role:"egress",public_address:"198.51.100.24",private_address:"10.24.0.3",public_interface:"eth0",private_interface:"wg0",status:"online",agent_version:"0.3.6",apply_status:"normal",last_seen_at:new Date().toISOString()},
  {id:"out-sg",name:"新加坡备用",role:"egress",public_address:"198.51.100.52",private_address:"10.24.0.4",public_interface:"ens3",private_interface:"wg0",status:"offline",last_seen_at:new Date(Date.now()-3600_000).toISOString()},
];
const demoLines:Line[] = [
  {id:"line-gz-hk",name:"广州 → 香港专线",mode:"dual_managed",ingress_node_id:"in-gz",egress_node_id:"out-hkg",egress_node_ids:["out-hkg","out-sg"],active_egress_node_id:"out-hkg",failover_enabled:true,listen_address:"0.0.0.0",relay_port_range:"",engine:"realm",ingress_engine:"nftables",egress_engine:"realm",enabled:true},
  {id:"line-hk-exit",name:"香港出口接管",mode:"exit_only",ingress_node_id:"out-hkg",egress_node_id:"out-hkg",egress_node_ids:["out-hkg"],active_egress_node_id:"out-hkg",failover_enabled:false,listen_address:"10.24.0.3",relay_port_range:"30000-39999",engine:"realm",ingress_engine:"realm",egress_engine:"realm",enabled:true},
];
const demoProbes:LinkProbe[]=[{ingress_node_id:"in-gz",egress_node_id:"out-hkg",address:"10.24.0.3",latency_ms:8.7,packet_loss:0,success:true,has_succeeded:true,failure_count:0,success_count:12,checked_at:new Date().toISOString()},{ingress_node_id:"in-gz",egress_node_id:"out-sg",address:"10.24.0.4",latency_ms:41.3,packet_loss:0,success:true,has_succeeded:true,failure_count:0,success_count:12,checked_at:new Date().toISOString()}];
const demoRules:Rule[] = [
  {id:"r1",line_id:"line-gz-hk",mode:"dual_managed",name:"游戏服务",protocol:"both",ingress_node_id:"in-gz",egress_node_id:"out-hkg",listen_address:"0.0.0.0",listen_port:24444,relay_port:32444,target_host:"192.0.2.88",target_port:24444,engine:"realm",ingress_engine:"nftables",egress_engine:"realm",upload_mbps:30,download_mbps:100,burst_kbytes:512,enabled:true},
  {id:"r2",line_id:"line-hk-exit",mode:"exit_only",name:"远程桌面",protocol:"both",ingress_node_id:"out-hkg",egress_node_id:"out-hkg",listen_address:"10.24.0.3",listen_port:33890,relay_port:33890,target_host:"192.0.2.91",target_port:3389,engine:"realm",ingress_engine:"realm",egress_engine:"realm",upload_mbps:10,download_mbps:40,burst_kbytes:256,enabled:true},
];
const demoTargetProbes:TargetProbe[]=[{rule_id:"r1",node_id:"out-hkg",address:"192.0.2.88",port:24444,latency_ms:28.4,packet_loss:0,success:true,has_succeeded:true,failure_count:0,success_count:12,tcp_checked:true,tcp_success:true,tcp_latency_ms:31.2,checked_at:new Date().toISOString()},{rule_id:"r2",node_id:"out-hkg",address:"192.0.2.91",port:3389,latency_ms:32.1,packet_loss:0,success:true,has_succeeded:true,failure_count:0,success_count:12,tcp_checked:true,tcp_success:false,tcp_latency_ms:0,tcp_error:"refused",checked_at:new Date().toISOString()}];
const demoHour=Math.floor(Date.now()/3600_000)*3600_000;
const demoPoints = Array.from({length:24},(_,i)=>({bucket:new Date(demoHour-(23-i)*3600_000).toISOString(),upload_bytes:(8+Math.sin(i/2)*4)*1024**3/24,download_bytes:(38+Math.cos(i/3)*12)*1024**3/24}));
const demoSummary:Summary={online_nodes:2,total_nodes:3,enabled_rules:2,total_rules:2,today_upload:8.4*1024**3,today_download:38.6*1024**3,week_upload:44*1024**3,week_download:196*1024**3,month_upload:178*1024**3,month_download:812*1024**3,quarter_upload:493*1024**3,quarter_download:2.3*1024**4,recent_traffic:demoPoints};
const demoRuleTraffic:RuleTraffic[]=[
  {rule_id:"r1",total_upload_bytes:428.7*1024**3,total_download_bytes:2.1*1024**4,today_upload_bytes:7.1*1024**3,today_download_bytes:32.4*1024**3,week_upload_bytes:38*1024**3,week_download_bytes:171*1024**3,month_upload_bytes:151*1024**3,month_download_bytes:704*1024**3,quarter_upload_bytes:420*1024**3,quarter_download_bytes:2*1024**4,upload_bytes_per_second:1.4*1024**2,download_bytes_per_second:5.8*1024**2},
  {rule_id:"r2",total_upload_bytes:98.2*1024**3,total_download_bytes:442.5*1024**3,today_upload_bytes:1.3*1024**3,today_download_bytes:6.2*1024**3,week_upload_bytes:6*1024**3,week_download_bytes:25*1024**3,month_upload_bytes:27*1024**3,month_download_bytes:108*1024**3,quarter_upload_bytes:73*1024**3,quarter_download_bytes:307*1024**3,upload_bytes_per_second:218*1024,download_bytes_per_second:890*1024},
];

const blankNode={name:"",role:"ingress" as NodeRole,public_address:"",private_address:"",public_interface:"",private_interface:""};
const blankLine={name:"",mode:"dual_managed" as Mode,ingress_node_id:"",egress_node_id:"",egress_node_ids:[] as string[],active_egress_node_id:"",failover_enabled:false,listen_address:"",relay_port_range:"",engine:"nftables" as Engine,ingress_engine:"nftables" as Engine,egress_engine:"nftables" as Engine,enabled:true};

async function api<T>(path:string,init?:RequestInit):Promise<T>{const response=await fetch(path,{credentials:"include",cache:"no-store",...init,headers:{"Content-Type":"application/json",...(init?.headers||{})}});if(!response.ok){let message=`请求失败 (${response.status})`;try{message=(await response.json()).error||message}catch{/* non-JSON */}throw new Error(message)}if(response.status===204)return undefined as T;return response.json()}
function bytes(value:number){if(!Number.isFinite(value))return "0 B";const units=["B","KB","MB","GB","TB"];let n=value,i=0;while(n>=1024&&i<units.length-1){n/=1024;i++}return `${n.toFixed(i>2?2:1)} ${units[i]}`}
function speed(value:number){return `${bytes(value)}/s`}
function roleName(role:NodeRole){return role==="ingress"?"入口":role==="egress"?"出口":"入口 / 出口"}
function nodeName(nodes:Node[],id:string){return nodes.find(n=>n.id===id)?.name||"未选择"}
function modeName(mode:Mode){return mode==="dual_managed"?"双端托管":"仅出口接管"}
function engineName(engine:Engine|undefined){return engine==="realm"?"Realm":"nftables"}
function lineEngineText(line:Pick<Line,"mode"|"engine"|"ingress_engine"|"egress_engine">){const ingress=line.ingress_engine||line.engine||"nftables",egress=line.egress_engine||line.engine||"nftables";return line.mode==="exit_only"?`出口 ${engineName(egress)}`:`入口 ${engineName(ingress)} → 出口 ${engineName(egress)}`}
function ruleEngineText(rule:Pick<Rule,"mode"|"engine"|"ingress_engine"|"egress_engine">){const ingress=rule.ingress_engine||rule.engine||"nftables",egress=rule.egress_engine||rule.engine||"nftables";return rule.mode==="exit_only"?`出口 ${engineName(egress)}`:`入口 ${engineName(ingress)} → 出口 ${engineName(egress)}`}
function tcpErrorName(value:string|undefined){return value==="timeout"?"连接超时":value==="refused"?"拒绝连接":value==="unreachable"?"网络不可达":value==="dns"?"DNS 失败":"连接失败"}
function probeCheckedTime(value:string|undefined){if(!value)return "未检查";const date=new Date(value);if(Number.isNaN(date.getTime()))return "时间未知";return new Intl.DateTimeFormat("zh-CN",{timeZone:"Asia/Shanghai",month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",second:"2-digit",hour12:false}).format(date)}
function asArray<T>(value:T[]|null|undefined):T[]{return Array.isArray(value)?value:[]}
function normalizeSummary(value:Summary):Summary{return {...value,recent_traffic:asArray(value?.recent_traffic)}}

async function copyText(value:string){
  if(window.navigator.clipboard&&window.isSecureContext){
    try{await window.navigator.clipboard.writeText(value);showCopyNotice("✓ 已复制");return}catch{/* HTTP/permission fallback below */}
  }
  const textarea=document.createElement("textarea");
  textarea.value=value;
  textarea.readOnly=true;
  textarea.style.position="fixed";
  textarea.style.left="-9999px";
  textarea.style.opacity="0";
  document.body.appendChild(textarea);
  textarea.focus();
  textarea.select();
  textarea.setSelectionRange(0,value.length);
  const copied=document.execCommand("copy");
  textarea.remove();
  if(!copied){showCopyNotice("复制失败，请手动复制",true);throw new Error("copy failed")}
  showCopyNotice("✓ 已复制");
}

function showCopyNotice(message:string,failed=false){
  let notice=document.getElementById("copy-notice");
  if(!notice){notice=document.createElement("div");notice.id="copy-notice";notice.setAttribute("role","status");notice.setAttribute("aria-live","polite");document.body.appendChild(notice)}
  notice.textContent=message;
  notice.className=failed?"copy-notice visible failed":"copy-notice visible";
  window.setTimeout(()=>notice?.classList.remove("visible"),2200);
}

function handleCopyClick(event:React.MouseEvent<HTMLElement>){
  const button=(event.target as HTMLElement).closest("button");
  if(!button||!(button.textContent?.includes("复制命令")||button.textContent?.includes("复制更新命令")||button.textContent?.includes("复制 Agent Token")))return;
  const value=button.closest(".command-box,.token-box")?.querySelector("code")?.textContent;
  if(!value)return;
  event.preventDefault();
  event.stopPropagation();
  void copyText(value);
}

export default function Home(){
  const [view,setView]=useState<View>("overview");
  const [themeMode,setThemeMode]=useState<ThemeMode>("system");
  const [authenticated,setAuthenticated]=useState<boolean|null>(null);
  const [demo,setDemo]=useState(false);
  const [nodes,setNodes]=useState<Node[]>([]),[lines,setLines]=useState<Line[]>([]),[rules,setRules]=useState<Rule[]>([]);
  const [summary,setSummary]=useState<Summary>(demoSummary),[traffic,setTraffic]=useState<Point[]>([]);
  const [trafficPeriod,setTrafficPeriod]=useState<TrafficPeriod>("day");
  const [ruleTraffic,setRuleTraffic]=useState<RuleTraffic[]>([]),[trafficRule,setTrafficRule]=useState<Rule|null>(null);
  const [probes,setProbes]=useState<LinkProbe[]>([]);
  const [targetProbes,setTargetProbes]=useState<TargetProbe[]>([]);
  const [error,setError]=useState(""),[busy,setBusy]=useState(false);
  const [nodeModal,setNodeModal]=useState(false),[lineModal,setLineModal]=useState(false),[ruleModal,setRuleModal]=useState(false);
  const [editingNode,setEditingNode]=useState<Node|null>(null),[editingLine,setEditingLine]=useState<Line|null>(null),[editingRule,setEditingRule]=useState<Rule|null>(null),[preferredLine,setPreferredLine]=useState("");
  const [updateNode,setUpdateNode]=useState<Node|null>(null);
  const [credential,setCredential]=useState<{nodeId:string;token:string}|null>(null);
  const [loginNotice,setLoginNotice]=useState("");
  useEffect(()=>{const saved=window.localStorage.getItem("relay-panel-theme");if(saved==="light"||saved==="dark"||saved==="system")window.setTimeout(()=>setThemeMode(saved),0)},[]);
  useEffect(()=>{const media=window.matchMedia("(prefers-color-scheme: dark)");const apply=()=>{const resolved=themeMode==="system"?(media.matches?"dark":"light"):themeMode;document.documentElement.dataset.theme=resolved;document.documentElement.dataset.themeMode=themeMode};apply();window.localStorage.setItem("relay-panel-theme",themeMode);media.addEventListener("change",apply);return()=>media.removeEventListener("change",apply)},[themeMode]);
  const load=useCallback(async()=>{try{await api("/api/v1/me")}catch{setAuthenticated(false);return}setAuthenticated(true);setDemo(false);try{const [n,l,r,s,t,rt,p,tp]=await Promise.all([api<Node[]>("/api/v1/nodes"),api<Line[]>("/api/v1/lines"),api<Rule[]>("/api/v1/rules"),api<Summary>("/api/v1/dashboard"),api<Point[]>("/api/v1/traffic?period=day"),api<RuleTraffic[]>("/api/v1/traffic/rules"),api<LinkProbe[]>("/api/v1/probes"),api<TargetProbe[]>("/api/v1/target-probes")]);setNodes(asArray(n));setLines(asArray(l));setRules(asArray(r));setSummary(normalizeSummary(s));setTraffic(asArray(t));setRuleTraffic(asArray(rt));setProbes(asArray(p));setTargetProbes(asArray(tp));setError("")}catch(e){setError(`控制台数据加载失败：${(e as Error).message}`)}},[]);
  // eslint-disable-next-line react-hooks/set-state-in-effect
  useEffect(()=>{void load()},[load]);
  useEffect(()=>{if(!authenticated||demo||(view!=="traffic"&&!trafficRule))return;const timer=window.setInterval(()=>{void api<RuleTraffic[]>("/api/v1/traffic/rules").then(value=>setRuleTraffic(asArray(value))).catch(()=>{})},10000);return()=>window.clearInterval(timer)},[authenticated,demo,view,trafficRule]);
  useEffect(()=>{if(!authenticated||demo||view!=="traffic")return;void api<Point[]>(`/api/v1/traffic?period=${trafficPeriod}`).then(value=>setTraffic(asArray(value))).catch(()=>{})},[authenticated,demo,view,trafficPeriod]);
  useEffect(()=>{if(!authenticated||demo||view!=="nodes")return;const update=()=>void api<Node[]>("/api/v1/nodes").then(value=>setNodes(asArray(value))).catch(()=>{});update();const timer=window.setInterval(update,5000);return()=>window.clearInterval(timer)},[authenticated,demo,view]);
  useEffect(()=>{if(!authenticated||demo||view!=="lines")return;const update=()=>void Promise.all([api<Node[]>("/api/v1/nodes"),api<Line[]>("/api/v1/lines"),api<LinkProbe[]>("/api/v1/probes")]).then(([n,l,p])=>{setNodes(asArray(n));setLines(asArray(l));setProbes(asArray(p))}).catch(()=>{});update();const timer=window.setInterval(update,10000);return()=>window.clearInterval(timer)},[authenticated,demo,view]);
  useEffect(()=>{if(!authenticated||demo||view!=="rules")return;const update=()=>void Promise.all([api<RuleTraffic[]>("/api/v1/traffic/rules"),api<TargetProbe[]>("/api/v1/target-probes")]).then(([rt,tp])=>{setRuleTraffic(asArray(rt));setTargetProbes(asArray(tp))}).catch(()=>{});update();const timer=window.setInterval(update,10000);return()=>window.clearInterval(timer)},[authenticated,demo,view]);
  const activateDemo=()=>{setNodes(demoNodes);setLines(demoLines);setRules(demoRules);setSummary(demoSummary);setTraffic(demoPoints);setRuleTraffic(demoRuleTraffic);setProbes(demoProbes);setTargetProbes(demoTargetProbes);setAuthenticated(true);setDemo(true)};
  if(authenticated===null)return <div className="boot"><span className="pulse"/>正在连接控制端</div>;
  if(!authenticated)return <Login notice={loginNotice} themeMode={themeMode} onThemeMode={setThemeMode} onSuccess={async()=>{setLoginNotice("");await load()}} onDemo={activateDemo}/>;
  const refresh=demo?async()=>{}:load;
  const openRule=(lineId="")=>{setEditingRule(null);setPreferredLine(lineId);setRuleModal(true)};
  const action=()=>{if(view==="nodes"){setEditingNode(null);setNodeModal(true)}else if(view==="lines"){setEditingLine(null);setLineModal(true)}else openRule()};
  const actionLabel=view==="nodes"?"接入服务器":view==="lines"?"创建线路":"新建转发";
  const showAction=view!=="settings";
  const logout=async()=>{if(!demo)await api("/api/v1/logout",{method:"POST"}).catch(()=>{});setAuthenticated(false);setDemo(false)};
  return <div className="shell" onClickCapture={handleCopyClick}>
    <Sidebar view={view} setView={setView} demo={demo} onLogout={logout}/>
    <main className="main">
      <header className="topbar"><div><p className="eyebrow">个人转发控制台</p><h1>{({overview:"网络总览",nodes:"服务器",lines:"线路",rules:"转发规则",traffic:"流量统计",settings:"系统设置"} as const)[view]}</h1></div><div className="top-actions"><ThemePicker value={themeMode} onChange={setThemeMode}/><span className="health"><i/>控制端正常</span>{showAction&&<button className="primary" onClick={action}>＋ {actionLabel}</button>}</div></header>
      {demo&&<div className="notice">当前为界面预览数据。服务器、线路和转发规则可直接操作体验。</div>}
      {error&&<div className="error">{error}<button onClick={()=>setError("")}>×</button></div>}
      {view==="overview"&&<Overview summary={summary} nodes={nodes} lines={lines} rules={rules} points={traffic.length?traffic:summary.recent_traffic} onOpenLines={()=>setView("lines")}/>}
      {view==="nodes"&&<Nodes nodes={nodes} onAdd={()=>{setEditingNode(null);setNodeModal(true)}} onEdit={n=>{setEditingNode(n);setNodeModal(true)}} onUpdate={setUpdateNode} onDelete={async id=>{if(demo){setNodes(v=>v.filter(n=>n.id!==id));return}if(confirm("确认删除该服务器？")){try{await api(`/api/v1/nodes/${id}`,{method:"DELETE"});await refresh()}catch(e){setError((e as Error).message)}}}}/>}
      {view==="lines"&&<Lines nodes={nodes} lines={lines} rules={rules} probes={probes} onAdd={()=>{setEditingLine(null);setLineModal(true)}} onEdit={line=>{setEditingLine(line);setLineModal(true)}} onRule={openRule} onDelete={async id=>{if(demo){setLines(v=>v.filter(l=>l.id!==id));return}if(confirm("确认删除该线路？线路下不能有转发规则。")){try{await api(`/api/v1/lines/${id}`,{method:"DELETE"});await refresh()}catch(e){setError((e as Error).message)}}}}/>}
      {view==="rules"&&<Rules
        nodes={nodes} lines={lines} rules={rules} traffic={ruleTraffic} targetProbes={targetProbes} onTraffic={setTrafficRule} onAdd={openRule}
        onEdit={rule=>{setEditingRule(rule);setPreferredLine(rule.line_id);setRuleModal(true)}}
        onToggle={async rule=>{if(demo){setRules(v=>v.map(r=>r.id===rule.id?{...r,enabled:!r.enabled}:r));return}await api(`/api/v1/rules/${rule.id}`,{method:"PUT",body:JSON.stringify({...rule,enabled:!rule.enabled})});await refresh()}}
        onDelete={async id=>{if(demo){setRules(v=>v.filter(r=>r.id!==id));return}if(confirm("确认删除该转发及其统计？")){await api(`/api/v1/rules/${id}`,{method:"DELETE"});await refresh()}}}
	    />}
      {view==="traffic"&&<Traffic
        rules={rules} ruleTraffic={ruleTraffic} points={traffic.length?traffic:summary.recent_traffic} summary={summary} period={trafficPeriod} onPeriod={setTrafficPeriod} onRule={setTrafficRule}
      />}
      {view==="settings"&&<Settings demo={demo} onConfigImported={refresh} onPasswordChanged={()=>{setLoginNotice("管理员密码已修改，请使用新密码重新登录。");setAuthenticated(false);setDemo(false)}}/>}
    </main>
    {nodeModal&&<NodeModal initial={editingNode} credential={credential} busy={busy} onClose={()=>{setNodeModal(false);setEditingNode(null);setCredential(null)}} onSave={async node=>{setBusy(true);try{if(demo){if(editingNode)setNodes(v=>v.map(n=>n.id===editingNode.id?{...n,...node}:n));else{const id=`demo-${Date.now()}`;setNodes(v=>[...v,{...node,id,status:"offline",last_seen_at:""}]);setCredential({nodeId:id,token:"demo_agent_token_only_shown_once"});return}}else if(editingNode){await api(`/api/v1/nodes/${editingNode.id}`,{method:"PUT",body:JSON.stringify(node)});await refresh()}else{const result=await api<{node:Node;agent_token:string}>("/api/v1/nodes",{method:"POST",body:JSON.stringify(node)});setCredential({nodeId:result.node.id,token:result.agent_token});await refresh();return}setNodeModal(false);setEditingNode(null)}catch(e){setError((e as Error).message)}finally{setBusy(false)}}}/>}
    {updateNode&&<AgentUpdateModal node={updateNode} onClose={()=>setUpdateNode(null)}/>}
    {lineModal&&<LineModal nodes={nodes} probes={probes} initial={editingLine} busy={busy} onClose={()=>{setLineModal(false);setEditingLine(null)}} onSave={async line=>{setBusy(true);try{if(demo){if(editingLine){setLines(v=>v.map(l=>l.id===editingLine.id?{...l,...line}:l));setRules(v=>v.map(r=>r.line_id===editingLine.id?{...r,mode:line.mode,ingress_node_id:line.mode==="exit_only"?line.egress_node_id:line.ingress_node_id,egress_node_id:line.egress_node_id,listen_address:line.listen_address,engine:line.egress_engine,ingress_engine:line.ingress_engine,egress_engine:line.egress_engine}:r))}else setLines(v=>[{...line,id:`demo-line-${Date.now()}`} as Line,...v])}else{await api(editingLine?`/api/v1/lines/${editingLine.id}`:"/api/v1/lines",{method:editingLine?"PUT":"POST",body:JSON.stringify(line)});await refresh()}setLineModal(false);setEditingLine(null);setView("lines")}catch(e){setError((e as Error).message)}finally{setBusy(false)}}}/>} {" "}
    {ruleModal&&<RuleModal
      lines={lines} nodes={nodes} initial={editingRule} initialLine={preferredLine} busy={busy}
      onNeedLine={()=>{setRuleModal(false);setEditingRule(null);setLineModal(true)}} onClose={()=>{setRuleModal(false);setEditingRule(null)}}
	      onSave={async draft=>{setBusy(true);try{const line=lines.find(l=>l.id===draft.line_id)!;if(demo){if(editingRule){setRules(v=>v.map(r=>r.id===editingRule.id?{...r,...draft,mode:line.mode,ingress_node_id:line.ingress_node_id,egress_node_id:line.egress_node_id,listen_address:line.listen_address||"0.0.0.0",relay_port:line.mode==="exit_only"?draft.listen_port:r.relay_port,engine:line.egress_engine,ingress_engine:line.ingress_engine,egress_engine:line.egress_engine}:r))}else{const rule:Rule={...draft,id:`demo-rule-${Date.now()}`,mode:line.mode,ingress_node_id:line.ingress_node_id,egress_node_id:line.egress_node_id,listen_address:line.listen_address||"0.0.0.0",relay_port:line.mode==="exit_only"?draft.listen_port:30000+draft.listen_port%30000,engine:line.egress_engine,ingress_engine:line.ingress_engine,egress_engine:line.egress_engine,enabled:true};setRules(v=>[rule,...v])}}else{await api(editingRule?`/api/v1/rules/${editingRule.id}`:"/api/v1/rules",{method:editingRule?"PUT":"POST",body:JSON.stringify(draft)});await refresh()}setRuleModal(false);setEditingRule(null);setView("rules")}catch(e){setError((e as Error).message)}finally{setBusy(false)}}}
    />}
    {trafficRule&&<RuleTrafficModal
      rule={trafficRule} stats={ruleTraffic.find(t=>t.rule_id===trafficRule.id)} demo={demo} onClose={()=>setTrafficRule(null)}
    />}
  </div>;
}

function Login({notice,themeMode,onThemeMode,onSuccess,onDemo}:{notice:string;themeMode:ThemeMode;onThemeMode:(mode:ThemeMode)=>void;onSuccess:()=>Promise<void>;onDemo:()=>void}){const [password,setPassword]=useState(""),[error,setError]=useState(""),[busy,setBusy]=useState(false);async function submit(e:FormEvent){e.preventDefault();setBusy(true);setError("");try{await api("/api/v1/login",{method:"POST",body:JSON.stringify({password})});await onSuccess()}catch(err){setError((err as Error).message)}finally{setBusy(false)}}return <div className="login-page"><div className="login-brand"><Logo/><div><b>Relay Panel</b><span>专线端口转发</span></div></div><div className="login-theme"><ThemePicker value={themeMode} onChange={onThemeMode}/></div><div className="login-card"><div className="login-mark"><span>RP</span></div><p className="eyebrow">安全管理入口</p><h1>欢迎回来</h1><p>登录以管理服务器、线路与转发规则。</p>{notice&&<div className="success-notice">{notice}</div>}<form onSubmit={submit}><label>管理员密码<input type="password" value={password} onChange={e=>setPassword(e.target.value)} placeholder="输入控制端密码"/></label>{error&&<div className="form-error">{error}</div>}<button className="primary wide" disabled={busy}>{busy?"正在登录…":"进入控制台"}</button></form><button className="text-button" onClick={onDemo}>预览管理界面 →</button></div><p className="login-foot">单管理员模式 · 本地数据 · Agent 加密鉴权</p></div>}
function Logo(){return <div className="logo"><span/><span/><span/></div>}
function BarChartIcon(){return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 19V10h4v9H4Zm6 0V5h4v14h-4Zm6 0v-7h4v7h-4Z" fill="currentColor"/><path d="M3 20h18" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round"/></svg>}
function ThemePicker({value,onChange}:{value:ThemeMode;onChange:(mode:ThemeMode)=>void}){const items:[ThemeMode,string,string][]=[["light","浅色","☀"],["dark","深色","☾"],["system","跟随系统","◐"]];return <div className="theme-picker" aria-label="页面主题">{items.map(([mode,label,icon])=><button key={mode} type="button" className={value===mode?"active":""} title={label} aria-label={label} onClick={()=>onChange(mode)}><span>{icon}</span><b>{label}</b></button>)}</div>}
function Sidebar({view,setView,demo,onLogout}:{view:View;setView:(v:View)=>void;demo:boolean;onLogout:()=>void}){const items:[View,string,string][]=[["overview","总览","⌁"],["nodes","服务器","◇"],["lines","线路","—"],["rules","转发规则","⇄"],["traffic","流量统计","chart"],["settings","系统设置","⚙"]];return <aside className="sidebar"><div className="brand"><Logo/><div><b>Relay Panel</b><span>Personal Edition</span></div></div><nav>{items.map(([key,label,icon])=><button key={key} className={view===key?"active":""} onClick={()=>setView(key)}><i>{icon==="chart"?<BarChartIcon/>:icon}</i>{label}</button>)}</nav><div className="sidebar-foot"><div className="mini-status"><i/><div><b>{demo?"预览模式":"系统运行中"}</b><span>{demo?"Demo data":"Agents ready"}</span></div></div><button className="logout-button" onClick={onLogout}>{demo?"退出预览":"退出登录"}</button><div className="version">在线更新 · 私人部署</div></div></aside>}

function Overview({summary,nodes,lines,rules,points,onOpenLines}:{summary:Summary;nodes:Node[];lines:Line[];rules:Rule[];points:Point[];onOpenLines:()=>void}){return <div className="page-grid"><section className="hero-panel"><div><p className="eyebrow">运行结构</p><h2>服务器、线路、转发各司其职</h2><p>服务器只接入一次；线路固定拓扑；日常转发只填写端口和落地。</p></div><div className="topology"><NodeDot label="本机"/><FlowLine/><NodeDot label="入口" active/><FlowLine privateLine/><NodeDot label="出口" active/><FlowLine/><NodeDot label="落地"/></div></section><section className="stats-row"><Metric label="今日总流量" value={bytes(summary.today_upload+summary.today_download)} note={`↑ ${bytes(summary.today_upload)} · ↓ ${bytes(summary.today_download)}`} tone="violet"/><Metric label="可用线路" value={String(lines.filter(l=>l.enabled).length)} note="固定链路配置" tone="cyan"/><Metric label="在线服务器" value={`${summary.online_nodes}/${summary.total_nodes}`} note={`${nodes.filter(n=>n.status!=="online").length} 台离线`} tone="green"/><Metric label="运行中转发" value={String(rules.filter(r=>r.enabled).length)} note={`共 ${rules.length} 条`} tone="amber"/></section><section className="chart-card"><div className="section-head"><div><p className="eyebrow">最近 24 小时</p><h3>流量趋势</h3></div><div className="legend"><span className="down">下载</span><span className="up">上传</span></div></div><TrafficChart points={points}/></section><section className="side-card"><div className="section-head"><div><p className="eyebrow">线路状态</p><h3>常用线路</h3></div></div><div className="line-mini-list">{lines.slice(0,4).map(l=><div key={l.id}><i className={nodes.find(n=>n.id===l.egress_node_id)?.status||"offline"}/><span><b>{l.name}</b><small>{modeName(l.mode)} · {lineEngineText(l)}</small></span></div>)}</div><button className="outline wide" onClick={onOpenLines}>管理线路</button></section></div>}
function NodeDot({label,active}:{label:string;active?:boolean}){return <div className={`topo-node ${active?"active":""}`}><span>{active?"●":"○"}</span><b>{label}</b></div>}
function FlowLine({privateLine}:{privateLine?:boolean}){return <div className={`topo-line ${privateLine?"private":""}`}><span>{privateLine?"内网":""}</span></div>}
function Metric({label,value,note,tone}:{label:string;value:string;note:string;tone:string}){return <div className={`metric ${tone}`}><span>{label}</span><strong>{value}</strong><small>{note}</small></div>}
function TrafficChart({ points }: { points: Point[] | null | undefined }) {
  const values = asArray(points).slice(-92),
    canvasRef = useRef<HTMLCanvasElement>(null),
    stageRef = useRef<HTMLDivElement>(null);
  const [mode, setMode] = useState<ChartMode>("line"),
    [hover, setHover] = useState<number | null>(null);
  const daily =
    values.length > 1 &&
    new Date(values[1].bucket).getTime() -
      new Date(values[0].bucket).getTime() >=
      20 * 3600_000;
  const label = useCallback(
    (bucket: string) =>
      new Intl.DateTimeFormat(
        "zh-CN",
        daily
          ? { timeZone: "Asia/Shanghai", month: "numeric", day: "numeric" }
          : {
              timeZone: "Asia/Shanghai",
              hour: "2-digit",
              minute: "2-digit",
              hour12: false,
            },
      ).format(new Date(bucket)),
    [daily],
  );
  useEffect(() => {
    const canvas = canvasRef.current,
      stage = stageRef.current;
    if (!canvas || !stage) return;
    const draw = () => {
      const width = Math.max(320, stage.clientWidth),
        height = 278,
        dpr = window.devicePixelRatio || 1;
      canvas.width = Math.round(width * dpr);
      canvas.height = Math.round(height * dpr);
      canvas.style.width = `${width}px`;
      canvas.style.height = `${height}px`;
      const ctx = canvas.getContext("2d");
      if (!ctx) return;
      const styles = window.getComputedStyle(stage),
        chartColor = (name:string,fallback:string) => styles.getPropertyValue(name).trim() || fallback,
        gridColor = chartColor("--chart-grid", "#202936"),
        axisColor = chartColor("--chart-axis", "#364151"),
        labelColor = chartColor("--chart-label", "#657286"),
        markerColor = chartColor("--chart-marker", "#10151e");
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, width, height);
      const left = 54,
        right = 18,
        top = 19,
        bottom = 36,
        plotW = width - left - right,
        plotH = height - top - bottom;
      const rawMax = Math.max(
        1,
        ...values.flatMap((p) => [p.upload_bytes, p.download_bytes]),
      );
      const units = ["B", "KB", "MB", "GB", "TB", "PB"];
      let unitIndex = 0,
        unitScale = 1;
      while (rawMax / unitScale >= 1024 && unitIndex < units.length - 1) {
        unitScale *= 1024;
        unitIndex++;
      }
      const displayMax = rawMax / unitScale,
        exponent = Math.pow(10, Math.floor(Math.log10(displayMax))),
        fraction = displayMax / exponent,
        niceDisplay =
          (fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10) *
          exponent,
        nice = niceDisplay * unitScale;
      ctx.font = '10px Inter,"PingFang SC",sans-serif';
      ctx.textBaseline = "middle";
      for (let i = 0; i <= 4; i++) {
        const y = top + (plotH * i) / 4,
          value = (nice * (4 - i)) / 4;
        ctx.strokeStyle = i === 4 ? axisColor : gridColor;
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.moveTo(left, y + 0.5);
        ctx.lineTo(width - right, y + 0.5);
        ctx.stroke();
        ctx.fillStyle = labelColor;
        ctx.textAlign = "right";
        ctx.fillText(
          (value / unitScale).toFixed(
            value === 0 ? 0 : value / unitScale < 10 ? 1 : 0,
          ),
          left - 10,
          y,
        );
      }
      ctx.fillStyle = labelColor;
      ctx.textAlign = "left";
      ctx.fillText(units[unitIndex], 8, top - 7);
      if (!values.length) {
        ctx.fillStyle = labelColor;
        ctx.textAlign = "center";
        ctx.fillText("当前周期暂无流量", left + plotW / 2, top + plotH / 2);
        return;
      }
      const xAt = (i: number) =>
          values.length === 1
            ? left + plotW / 2
            : left + (plotW * i) / (values.length - 1),
        yAt = (value: number) =>
          top + plotH - Math.max(0, Math.min(1, value / nice)) * plotH;
      const maxLabels = Math.max(2, Math.floor(plotW / 64)),
        labelEvery = Math.max(1, Math.ceil(values.length / maxLabels));
      values.forEach((point, i) => {
        const last = values.length - 1;
        if (
          i !== 0 &&
          i !== last &&
          (i % labelEvery !== 0 || last - i < labelEvery)
        )
          return;
        ctx.fillStyle = labelColor;
        ctx.textAlign =
          i === 0 ? "left" : i === values.length - 1 ? "right" : "center";
        ctx.fillText(label(point.bucket), xAt(i), height - 14);
      });
      const series = [
        { key: "download_bytes" as const, color: chartColor("--chart-download", "#806cff") },
        { key: "upload_bytes" as const, color: chartColor("--chart-upload", "#24c9d5") },
      ];
      if (mode === "bar") {
        const group = Math.max(
            5,
            Math.min(32, (plotW / Math.max(values.length, 1)) * 0.72),
          ),
          bar = Math.max(2, (group - 2) / 2);
        series.forEach((item, s) =>
          values.forEach((point, i) => {
            const x = xAt(i) - group / 2 + s * (bar + 2),
              y = yAt(point[item.key]);
            ctx.fillStyle = item.color;
            ctx.globalAlpha = 0.9;
            ctx.fillRect(x, y, bar, top + plotH - y);
          }),
        );
        ctx.globalAlpha = 1;
      } else {
        series.forEach((item) => {
          const coords = values.map((point, i) => ({
            x: xAt(i),
            y: yAt(point[item.key]),
          }));
          const gradient = ctx.createLinearGradient(0, top, 0, top + plotH);
          gradient.addColorStop(0, item.color + "2b");
          gradient.addColorStop(1, item.color + "00");
          ctx.beginPath();
          ctx.moveTo(coords[0].x, top + plotH);
          coords.forEach((p, i) => {
            if (i === 0) ctx.lineTo(p.x, p.y);
            else {
              const prev = coords[i - 1],
                mid = (prev.x + p.x) / 2;
              ctx.bezierCurveTo(mid, prev.y, mid, p.y, p.x, p.y);
            }
          });
          ctx.lineTo(coords.at(-1)!.x, top + plotH);
          ctx.closePath();
          ctx.fillStyle = gradient;
          ctx.fill();
          ctx.beginPath();
          coords.forEach((p, i) => {
            if (i === 0) ctx.moveTo(p.x, p.y);
            else {
              const prev = coords[i - 1],
                mid = (prev.x + p.x) / 2;
              ctx.bezierCurveTo(mid, prev.y, mid, p.y, p.x, p.y);
            }
          });
          ctx.strokeStyle = item.color;
          ctx.lineWidth = 2;
          ctx.lineJoin = "round";
          ctx.stroke();
        });
      }
      if (hover !== null && values[hover]) {
        const x = xAt(hover);
        ctx.strokeStyle = chartColor("--chart-hover", "#8190a755");
        ctx.setLineDash([4, 4]);
        ctx.beginPath();
        ctx.moveTo(x, top);
        ctx.lineTo(x, top + plotH);
        ctx.stroke();
        ctx.setLineDash([]);
        series.forEach((item) => {
          ctx.beginPath();
          ctx.arc(x, yAt(values[hover][item.key]), 4, 0, Math.PI * 2);
          ctx.fillStyle = markerColor;
          ctx.fill();
          ctx.lineWidth = 2;
          ctx.strokeStyle = item.color;
          ctx.stroke();
        });
      }
    };
    draw();
    const observer = new ResizeObserver(draw);
    observer.observe(stage);
    return () => observer.disconnect();
  }, [values, mode, hover, label]);
  const move = (event: React.PointerEvent<HTMLDivElement>) => {
    if (!values.length) return;
    const rect = event.currentTarget.getBoundingClientRect(),
      left = 54,
      right = 18,
      usable = Math.max(1, rect.width - left - right),
      index = Math.round(
        ((event.clientX - rect.left - left) / usable) * (values.length - 1),
      );
    setHover(Math.max(0, Math.min(values.length - 1, index)));
  };
  const active = hover === null ? null : values[hover];
  return (
    <div className="traffic-chart">
      <div className="chart-toolbar">
        <div className="chart-legend">
          <span className="download">下载</span>
          <span className="upload">上传</span>
        </div>
        <div className="chart-mode" aria-label="图表类型">
          <button
            className={mode === "line" ? "active" : ""}
            onClick={() => setMode("line")}
          >
            折线图
          </button>
          <button
            className={mode === "bar" ? "active" : ""}
            onClick={() => setMode("bar")}
          >
            柱形图
          </button>
        </div>
      </div>
      <div
        className="traffic-chart-stage"
        ref={stageRef}
        onPointerMove={move}
        onPointerLeave={() => setHover(null)}
      >
        <canvas ref={canvasRef} role="img" aria-label="上传和下载流量趋势图" />
        {active && (
          <div
            className="chart-tooltip"
            style={{
              left: `clamp(78px, ${values.length === 1 ? 50 : ((hover || 0) / (values.length - 1)) * 100}%, calc(100% - 78px))`,
            }}
          >
            <b>{label(active.bucket)}</b>
            <span className="download">
              下载 {bytes(active.download_bytes)}
            </span>
            <span className="upload">上传 {bytes(active.upload_bytes)}</span>
          </div>
        )}
      </div>
    </div>
  );
}

function Nodes({
  nodes,
  onAdd,
  onEdit,
  onUpdate,
  onDelete,
}: {
  nodes: Node[];
  onAdd: () => void;
  onEdit: (n: Node) => void;
  onUpdate: (n: Node) => void;
  onDelete: (id: string) => void;
}) {
  return (
    <div className="resource-page">
      <section className="workflow-strip">
        <span>1</span>
        <div>
          <b>在面板创建服务器</b>
          <small>只需要名称和用途</small>
        </div>
        <i />
        <span>2</span>
        <div>
          <b>复制一键安装命令</b>
          <small>在服务器执行一次</small>
        </div>
        <i />
        <span>3</span>
        <div>
          <b>Agent 上线</b>
          <small>再用于创建线路</small>
        </div>
      </section>
      <section className="table-card">
        <div className="section-head">
          <div>
            <p className="eyebrow">{nodes.length} 台服务器</p>
            <h3>已接入服务器</h3>
          </div>
          <button className="outline" onClick={onAdd}>
            ＋ 接入服务器
          </button>
        </div>
        <div className="node-cards">
          {nodes.map((n) => (
            <article
              key={n.id}
              className={n.apply_status === "failed" ? "apply-failed" : ""}
            >
              <div className="node-card-top">
                <span className={`node-icon large ${n.status}`}>
                  {n.role === "ingress"
                    ? "IN"
                    : n.role === "egress"
                      ? "OUT"
                      : "I/O"}
                </span>
                <em
                  className={n.apply_status === "failed" ? "failed" : n.status}
                >
                  {n.apply_status === "failed"
                    ? "下发失败"
                    : n.status === "online"
                      ? "在线"
                      : "等待连接"}
                </em>
              </div>
              <h3>{n.name}</h3>
              <p>
                {roleName(n.role)}服务器
                {n.agent_version ? ` · Agent ${n.agent_version}` : ""}
              </p>
              {n.apply_status === "failed" && (
                <div className="apply-error">
                  <b>配置未生效</b>
                  <span>{n.apply_error || "请检查 Agent 日志后重试"}</span>
                </div>
              )}
              <dl>
                <div>
                  <dt>公网</dt>
                  <dd>{n.public_address || "待配置"}</dd>
                </div>
                <div>
                  <dt>内网</dt>
                  <dd>{n.private_address || "待配置"}</dd>
                </div>
                <div>
                  <dt>网卡</dt>
                  <dd>
                    {n.public_interface || "—"} / {n.private_interface || "—"}
                  </dd>
                </div>
              </dl>
              <div className="card-actions">
                <span className="secondary-actions">
                  <button className="outline" onClick={() => onEdit(n)}>
                    配置
                  </button>
                  <button className="outline" onClick={() => onUpdate(n)}>
                    更新 Agent
                  </button>
                </span>
                <button className="danger-link" onClick={() => onDelete(n.id)}>
                  删除
                </button>
              </div>
            </article>
          ))}
        </div>
        {!nodes.length && (
          <Empty
            title="还没有服务器"
            description="先创建服务器，面板会给出一键安装命令。"
            action={onAdd}
          />
        )}
      </section>
    </div>
  );
}

function Lines({nodes,lines,rules,probes,onAdd,onEdit,onRule,onDelete}:{nodes:Node[];lines:Line[];rules:Rule[];probes:LinkProbe[];onAdd:()=>void;onEdit:(line:Line)=>void;onRule:(id:string)=>void;onDelete:(id:string)=>void}){return <section className="table-card"><div className="section-head"><div><p className="eyebrow">延迟探测 · 多出口自动切换</p><h3>线路</h3></div><button className="outline" onClick={onAdd}>＋ 创建线路</button></div><div className="line-grid">{lines.map(line=>{const ingress=nodes.find(n=>n.id===line.ingress_node_id),activeID=line.active_egress_node_id||line.egress_node_id,egress=nodes.find(n=>n.id===activeID),count=rules.filter(r=>r.line_id===line.id).length,probe=probes.find(p=>p.ingress_node_id===line.ingress_node_id&&p.egress_node_id===activeID),exitCount=(line.egress_node_ids?.length||1),probeClass=!probe||!probe.has_succeeded?"unknown":probe.packet_loss>=100?"bad":probe.packet_loss>0?"warn":"good";return <article className="line-card" key={line.id}><div className="line-card-head"><div><span className="line-badge">{modeName(line.mode)}</span>{line.failover_enabled&&<span className="line-badge failover">自动主备</span>}<h3>{line.name}</h3><p>{lineEngineText(line)} · {count} 条转发 · {exitCount} 个出口 · 端口 {line.relay_port_range||"不限制"}</p></div><i className={egress?.status||"offline"}/></div>{line.mode==="dual_managed"&&<div className={`link-quality ${probeClass}`}><span>当前出口：<b>{egress?.name||"未选择"}</b></span><span>{probe?.has_succeeded?`延迟 ${probe.latency_ms.toFixed(1)} ms · 丢包 ${probe.packet_loss.toFixed(0)}%`:probe?"ICMP 不可用 · 按 Agent 在线状态判断":"等待入口 Agent 探测"}</span></div>}<div className="route-visual"><div><small>{line.mode==="dual_managed"?`公网入口 · ${engineName(line.ingress_engine||line.engine)}`:"已有入口"}</small><b>{line.mode==="dual_managed"?ingress?.name:"已有入口规则"}</b><code>{line.mode==="dual_managed"?(ingress?.public_address||"待配置"):"不由面板管理"}</code></div><span>→</span><div><small>{line.failover_enabled?`当前活动出口 · ${engineName(line.egress_engine||line.engine)}`:`出口接管 · ${engineName(line.egress_engine||line.engine)}`}</small><b>{egress?.name}</b><code>{line.mode==="exit_only"?(line.listen_address||egress?.private_address||"待配置"):(egress?.private_address||"待配置")}</code></div><span>→</span><div><small>规则目标</small><b>落地服务器</b><code>每条转发填写</code></div></div><div className="card-actions"><button className="primary" onClick={()=>onRule(line.id)}>＋ 在此线路新建转发</button><span className="secondary-actions"><button className="outline" onClick={()=>onEdit(line)}>修改线路</button><button className="danger-link" onClick={()=>onDelete(line.id)}>删除</button></span></div></article>})}</div>{!lines.length&&<Empty title="还没有线路" description="把入口、出口和各自的转发引擎组合成一条可复用线路。" action={onAdd}/>}</section>}

function Rules({nodes,lines,rules,traffic,targetProbes,onTraffic,onAdd,onEdit,onToggle,onDelete}:{nodes:Node[];lines:Line[];rules:Rule[];traffic:RuleTraffic[];targetProbes:TargetProbe[];onTraffic:(r:Rule)=>void;onAdd:(id?:string)=>void;onEdit:(r:Rule)=>void;onToggle:(r:Rule)=>void;onDelete:(id:string)=>void}){const groups=lines.map(line=>({line,rules:rules.filter(r=>r.line_id===line.id)}));const legacy=rules.filter(r=>!r.line_id||!lines.some(l=>l.id===r.line_id));return <div className="rule-groups"><div className="section-head standalone"><div><p className="eyebrow">先选线路，再填端口与落地</p><h3>全部转发</h3></div><button className="outline" onClick={()=>onAdd()}>＋ 新建转发</button></div>{groups.map(({line,rules:items})=><section className="rule-group" key={line.id}><header><div><span className="line-badge">{modeName(line.mode)}</span>{line.failover_enabled&&<span className="line-badge failover">自动主备</span>}<h3>{line.name}</h3><p>{line.mode==="exit_only"?"已有入口":nodeName(nodes,line.ingress_node_id)} → {nodeName(nodes,line.active_egress_node_id||line.egress_node_id)} · {lineEngineText(line)}</p></div><button className="outline" onClick={()=>onAdd(line.id)}>＋ 添加转发</button></header>{items.length?<RuleTable rules={items} lines={lines} traffic={traffic} targetProbes={targetProbes} onTraffic={onTraffic} onEdit={onEdit} onToggle={onToggle} onDelete={onDelete}/>:<div className="group-empty">这条线路还没有转发规则</div>}</section>)}{legacy.length>0&&<section className="rule-group"><header><div><span className="line-badge muted">旧规则</span><h3>未归入线路</h3><p>升级前创建的规则仍可正常运行</p></div></header><RuleTable rules={legacy} lines={lines} traffic={traffic} targetProbes={targetProbes} onTraffic={onTraffic} onEdit={onEdit} onToggle={onToggle} onDelete={onDelete}/></section>}{!rules.length&&!lines.length&&<Empty title="还没有可用线路" description="创建线路后，转发规则将只需填写端口与落地。" action={()=>onAdd()}/>}</div>}
function RuleTable({rules,lines,traffic,targetProbes,onTraffic,onEdit,onToggle,onDelete}:{rules:Rule[];lines:Line[];traffic:RuleTraffic[];targetProbes:TargetProbe[];onTraffic:(r:Rule)=>void;onEdit:(r:Rule)=>void;onToggle:(r:Rule)=>void;onDelete:(id:string)=>void}){return <div className="data-table"><div className="table-row table-labels"><span>规则</span><span>监听</span><span>落地</span><span>业务健康</span><span>规则流量</span><span>限速</span><span>状态</span><span/></div>{rules.map(r=>{const t=traffic.find(item=>item.rule_id===r.id);const line=lines.find(item=>item.id===r.line_id);const activeEgress=line?.active_egress_node_id||r.egress_node_id;const p=targetProbes.find(item=>item.rule_id===r.id&&item.node_id===activeEgress)||targetProbes.find(item=>item.rule_id===r.id);const tcpExpected=r.protocol!=="udp";const quality=p?(tcpExpected&&p.tcp_checked?(p.tcp_success?"good":"bad"):(p.success?(p.packet_loss>0?"warn":"good"):(p.has_succeeded?"bad":"warn"))):"";const healthTitle=!p?"等待出口探测":tcpExpected?(p.tcp_checked?(p.tcp_success?`TCP 正常 · ${p.tcp_latency_ms.toFixed(1)} ms`:`TCP ${tcpErrorName(p.tcp_error)}`):"等待新版 Agent 探测 TCP"):(p.success?`ICMP ${p.latency_ms.toFixed(1)} ms`:p.has_succeeded?"ICMP 当前不可达":"ICMP 不可用");const icmpText=p?(p.success?`网络 ${p.latency_ms.toFixed(1)} ms / 丢包 ${p.packet_loss.toFixed(0)}%`:p.has_succeeded?`网络当前不可达 / 丢包 ${p.packet_loss.toFixed(0)}%`:`网络 ICMP 不可用 / 丢包 ${p.packet_loss.toFixed(0)}%`):"出口 Agent 尚未上报";return <div className="table-row" key={r.id}><span><i className={`protocol ${r.protocol}`}>{r.protocol.toUpperCase()}</i><b>{r.name}</b><small>{ruleEngineText(r)}</small></span><span><b>:{r.listen_port}</b><small>{r.mode==="dual_managed"?"公网入口":"内网接入"}</small></span><span><b>{r.target_host}:{r.target_port}</b><small>中继端口 {r.relay_port}</small></span><span className={`target-quality ${quality}`} title={r.protocol==="both"?"TCP 检查只代表 TCP 业务端口；UDP 仍以实际业务测试为准":undefined}><b>{healthTitle}</b><small>{icmpText}</small><small>{p?`检查于 ${probeCheckedTime(p.checked_at)}（北京时间）`:"通常 10–20 秒内上报"}</small></span><span className="rule-traffic"><b>↓ {bytes(t?.total_download_bytes||0)} · ↑ {bytes(t?.total_upload_bytes||0)}</b><small>实时 ↓ {speed(t?.download_bytes_per_second||0)} · ↑ {speed(t?.upload_bytes_per_second||0)}</small></span><span><b>↓ {r.download_mbps||"∞"} Mbps</b><small>↑ {r.upload_mbps||"∞"} Mbps</small></span><span><button className={`switch ${r.enabled?"on":""}`} aria-label={r.enabled?"暂停规则":"启用规则"} onClick={()=>onToggle(r)}><i/></button><small>{r.enabled?"运行中":"已暂停"}</small></span><span className="row-actions"><button className="icon-button" title="修改转发" aria-label="修改转发" onClick={()=>onEdit(r)}>✎</button><button className="icon-button" title="查看流量详情" aria-label="查看流量详情" onClick={()=>onTraffic(r)}>▥</button><button className="icon-button danger" aria-label="删除规则" onClick={()=>onDelete(r.id)}>×</button></span></div>})}</div>}

const periodNames:Record<TrafficPeriod,string>={day:"今日",week:"本周",month:"本月",quarter:"本季度"};
function PeriodPicker({value,onChange}:{value:TrafficPeriod;onChange:(period:TrafficPeriod)=>void}){return <div className="period-picker">{(Object.keys(periodNames) as TrafficPeriod[]).map(period=><button key={period} className={value===period?"active":""} onClick={()=>onChange(period)}>{periodNames[period]}</button>)}</div>}
function periodTotal(stats:RuleTraffic|undefined,period:TrafficPeriod){if(!stats)return 0;switch(period){case"week":return stats.week_upload_bytes+stats.week_download_bytes;case"month":return stats.month_upload_bytes+stats.month_download_bytes;case"quarter":return stats.quarter_upload_bytes+stats.quarter_download_bytes;default:return stats.today_upload_bytes+stats.today_download_bytes}}
function Traffic({rules,ruleTraffic,points,summary,period,onPeriod,onRule}:{rules:Rule[];ruleTraffic:RuleTraffic[];points:Point[];summary:Summary;period:TrafficPeriod;onPeriod:(period:TrafficPeriod)=>void;onRule:(rule:Rule)=>void}){const totals={day:[summary.today_upload,summary.today_download],week:[summary.week_upload,summary.week_download],month:[summary.month_upload,summary.month_download],quarter:[summary.quarter_upload,summary.quarter_download]} as const;return <div className="traffic-page"><section className="stats-row period-stats">{(Object.keys(periodNames) as TrafficPeriod[]).map((key,index)=>{const [up,down]=totals[key];return <Metric key={key} label={periodNames[key]} value={bytes(up+down)} note={`↑ ${bytes(up)} · ↓ ${bytes(down)}`} tone={["violet","cyan","green","amber"][index]}/>} )}</section><section className="chart-card full"><div className="section-head"><div><p className="eyebrow">流量趋势 · 北京时间自然周期</p><h3>全规则趋势</h3></div><PeriodPicker value={period} onChange={onPeriod}/></div><TrafficChart points={points}/></section><section className="table-card"><div className="section-head"><div><p className="eyebrow">逐条统计 · 点击规则查看独立趋势</p><h3>规则流量</h3></div><span className="tag">实时速率每 10 秒刷新</span></div><div className="data-table traffic-table">{rules.map(r=>{const t=ruleTraffic.find(item=>item.rule_id===r.id);const open=()=>onRule(r);return <div className="table-row traffic-rule-row" key={r.id} role="button" tabIndex={0} aria-label={`查看 ${r.name} 的流量趋势`} onClick={open} onKeyDown={event=>{if(event.key==="Enter"||event.key===" "){event.preventDefault();open()}}}><span><i className={`protocol ${r.protocol}`}>{r.protocol.toUpperCase()}</i><b>{r.name}</b><small>点击查看折线图与柱形图</small></span><span><small>实时速率</small><b>↓ {speed(t?.download_bytes_per_second||0)}</b><small>↑ {speed(t?.upload_bytes_per_second||0)}</small></span><span><small>今日</small><b>{bytes(periodTotal(t,"day"))}</b></span><span><small>本周</small><b>{bytes(periodTotal(t,"week"))}</b></span><span><small>本月</small><b>{bytes(periodTotal(t,"month"))}</b></span><span><small>本季度</small><b>{bytes(periodTotal(t,"quarter"))}</b></span><span className="traffic-open"><BarChartIcon/><b>查看趋势</b></span></div>})}{!rules.length&&<div className="group-empty">还没有可以统计的转发规则</div>}</div></section></div>}
function Settings({demo,onPasswordChanged,onConfigImported}:{demo:boolean;onPasswordChanged:()=>void;onConfigImported:()=>Promise<void>}){
  const [currentPassword,setCurrentPassword]=useState(""),[newPassword,setNewPassword]=useState(""),[confirmPassword,setConfirmPassword]=useState("");
  const [error,setError]=useState(""),[busy,setBusy]=useState(false),[configBusy,setConfigBusy]=useState(false),[configNotice,setConfigNotice]=useState(""),[configError,setConfigError]=useState("");
  const configInput=useRef<HTMLInputElement>(null);
  const updateCommand="curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update.sh | sudo bash";
  async function changePassword(e:FormEvent){e.preventDefault();setError("");if(demo){setError("预览模式不会修改管理员密码");return}if(newPassword.length<10){setError("新密码至少需要 10 个字符");return}if(newPassword!==confirmPassword){setError("两次输入的新密码不一致");return}setBusy(true);try{await api("/api/v1/admin/password",{method:"POST",body:JSON.stringify({current_password:currentPassword,new_password:newPassword})});onPasswordChanged()}catch(err){setError((err as Error).message)}finally{setBusy(false)}}
  async function exportConfig(){
    setConfigBusy(true);setConfigError("");setConfigNotice("");
    try{
      let blob:Blob,filename=`relay-panel-config-${new Date().toISOString().slice(0,10)}.json`;
      if(demo){const ids=new Set([...demoLines.flatMap(line=>[line.ingress_node_id,...line.egress_node_ids]),...demoRules.flatMap(rule=>[rule.ingress_node_id,rule.egress_node_id])]);const backup:ConfigBackup={format:"relay-panel-configuration",schema_version:1,exported_at:new Date().toISOString(),required_nodes:demoNodes.filter(node=>ids.has(node.id)).map(({id,name,role})=>({id,name,role})),lines:demoLines,rules:demoRules};blob=new Blob([JSON.stringify(backup,null,2)],{type:"application/json"})}
      else{const response=await fetch("/api/v1/config/export",{credentials:"include",cache:"no-store"});if(!response.ok){let message=`导出失败 (${response.status})`;try{message=(await response.json()).error||message}catch{/* non-JSON */}throw new Error(message)}blob=await response.blob();const match=response.headers.get("Content-Disposition")?.match(/filename="([^"]+)"/);if(match)filename=match[1]}
      const url=URL.createObjectURL(blob),anchor=document.createElement("a");anchor.href=url;anchor.download=filename;document.body.appendChild(anchor);anchor.click();anchor.remove();URL.revokeObjectURL(url);setConfigNotice(`已下载配置：${filename}`);
    }catch(err){setConfigError((err as Error).message)}finally{setConfigBusy(false)}
  }
  async function importConfig(file:File){
    setConfigBusy(true);setConfigError("");setConfigNotice("");
    try{
      if(file.size>1024*1024)throw new Error("配置文件不能超过 1 MB");
      const parsed=JSON.parse(await file.text()) as ConfigBackup;
      if(demo){setConfigNotice(`预览模式已读取文件：${parsed.lines?.length||0} 条线路、${parsed.rules?.length||0} 条规则，不会写入。`);return}
      const body=JSON.stringify(parsed);
      const preview=await api<ConfigImportResult>("/api/v1/config/import?dry_run=1",{method:"POST",body});
      if(!window.confirm(`预检通过：将合并 ${preview.lines} 条线路和 ${preview.rules} 条规则。未包含的当前配置不会删除，是否继续？`)){setConfigNotice("已取消上传，当前配置未改变。");return}
      const result=await api<ConfigImportResult>("/api/v1/config/import",{method:"POST",body});
      await onConfigImported();setConfigNotice(`恢复完成：已合并 ${result.lines} 条线路和 ${result.rules} 条规则。`);
    }catch(err){setConfigError(err instanceof SyntaxError?"配置文件不是有效的 JSON":(err as Error).message)}finally{setConfigBusy(false);if(configInput.current)configInput.current.value=""}
  }
  return <div className="settings-grid">
    <section className="settings-card"><div className="section-head"><div><p className="eyebrow">管理员账户</p><h3>修改登录密码</h3></div><span className="settings-icon">⌁</span></div><p>修改成功后会注销全部管理会话，服务器 Agent 和现有转发不受影响。</p><form className="settings-form" onSubmit={changePassword}><label>当前密码<input required type="password" autoComplete="current-password" value={currentPassword} onChange={e=>setCurrentPassword(e.target.value)}/></label><label>新密码<input required minLength={10} type="password" autoComplete="new-password" value={newPassword} onChange={e=>setNewPassword(e.target.value)}/><small>至少 10 个字符</small></label><label>再次输入新密码<input required minLength={10} type="password" autoComplete="new-password" value={confirmPassword} onChange={e=>setConfirmPassword(e.target.value)}/></label>{error&&<div className="form-error">{error}</div>}<button className="primary" disabled={busy}>{busy?"修改中…":"修改密码"}</button></form></section>
    <section className="settings-card"><div className="section-head"><div><p className="eyebrow">GitHub 在线更新</p><h3>更新主控</h3></div><span className="settings-icon">↻</span></div><p>只更新网页端与控制端，保留数据库、管理员密码、面板端口、HTTPS 反代、服务器、线路、规则和流量记录。</p><div className="update-points"><span>✓ 自动下载并校验最新版</span><span>✓ 新版健康检查失败自动回滚</span><span>✓ 不需要重新运行安装脚本</span></div><div className="command-box"><code>{updateCommand}</code><button onClick={()=>void copyText(updateCommand)}>复制更新命令</button></div><small className="settings-hint">在主控服务器 SSH 中执行。已安装 Agent 可继续使用各服务器卡片中的“更新 Agent”。</small></section>
    <section className="settings-card config-card"><div className="section-head"><div><p className="eyebrow">线路与规则</p><h3>转发配置备份</h3></div><span className="settings-icon">⇅</span></div><p>下载线路、双端引擎、端口池和转发规则；上传时先检查服务器引用和端口冲突，再按 ID 安全合并。</p><div className="update-points"><span>✓ 不包含管理员密码和 Agent Token</span><span>✓ 不包含流量统计与历史探测记录</span><span>✓ 上传不会删除文件之外的当前配置</span></div>{configNotice&&<div className="success-notice" role="status">{configNotice}</div>}{configError&&<div className="form-error" role="alert">{configError}</div>}<div className="config-actions"><button className="primary" type="button" disabled={configBusy} onClick={()=>void exportConfig()}>{configBusy?"处理中…":"下载配置文件"}</button><button className="outline" type="button" disabled={configBusy} onClick={()=>configInput.current?.click()}>上传并合并</button><input ref={configInput} className="config-input" type="file" accept=".json,application/json" onChange={event=>{const file=event.target.files?.[0];if(file)void importConfig(file)}}/></div><small className="settings-hint">恢复前，相同 ID 的服务器必须仍在面板中；缺失服务器或端口冲突会在写入前拦截。</small></section>
  </div>
}
function RuleTrafficModal({rule,stats,demo,onClose}:{rule:Rule;stats?:RuleTraffic;demo:boolean;onClose:()=>void}){const [points,setPoints]=useState<Point[]>(demo?demoPoints:[]),[period,setPeriod]=useState<TrafficPeriod>("day"),[loading,setLoading]=useState(!demo);const changePeriod=(value:TrafficPeriod)=>{if(value!==period&&!demo)setLoading(true);setPeriod(value)};useEffect(()=>{if(demo)return;void api<Point[]>(`/api/v1/traffic?rule_id=${encodeURIComponent(rule.id)}&period=${period}`).then(value=>setPoints(asArray(value))).catch(()=>setPoints([])).finally(()=>setLoading(false))},[demo,rule.id,period]);return <Modal title={rule.name} kicker="规则流量详情" onClose={onClose}><div className="rule-period-grid">{(Object.keys(periodNames) as TrafficPeriod[]).map((key,index)=><button key={key} className={period===key?"active":""} onClick={()=>changePeriod(key)}><Metric label={periodNames[key]} value={bytes(periodTotal(stats,key))} note={key===period?"正在查看此周期":"点击查看趋势"} tone={["violet","cyan","green","amber"][index]}/></button>)}</div><div className="traffic-detail-summary"><span><small>累计流量</small><b>{bytes((stats?.total_upload_bytes||0)+(stats?.total_download_bytes||0))}</b><em>↑ {bytes(stats?.total_upload_bytes||0)} · ↓ {bytes(stats?.total_download_bytes||0)}</em></span><span><small>当前速率</small><b>↓ {speed(stats?.download_bytes_per_second||0)}</b><em>↑ {speed(stats?.upload_bytes_per_second||0)}</em></span></div><div className="traffic-detail-chart"><div className="section-head"><div><p className="eyebrow">北京时间自然周期</p><h3>{periodNames[period]}流量趋势</h3></div><PeriodPicker value={period} onChange={changePeriod}/></div>{loading?<div className="group-empty">正在读取统计…</div>:<TrafficChart points={points}/>}</div></Modal>}
function Empty({title,description,action}:{title:string;description:string;action:()=>void}){return <div className="empty"><span>＋</span><h3>{title}</h3><p>{description}</p><button className="primary" onClick={action}>开始配置</button></div>}

function NodeModal({initial,credential,busy,onClose,onSave}:{initial:Node|null;credential:{nodeId:string;token:string}|null;busy:boolean;onClose:()=>void;onSave:(n:typeof blankNode)=>void}){const [n,setN]=useState(()=>initial?{name:initial.name,role:initial.role,public_address:initial.public_address,private_address:initial.private_address,public_interface:initial.public_interface||"eth0",private_interface:initial.private_interface||"wg0"}:blankNode);const field=(key:keyof typeof n)=>(e:React.ChangeEvent<HTMLInputElement|HTMLSelectElement>)=>setN({...n,[key]:e.target.value} as typeof n);const installCommand=credential?`curl -fsSL https://raw.githubusercontent.com/yuanziiiii/Realm/main/scripts/install-agent-online.sh | sudo bash -s -- --controller ${typeof window!=="undefined"?window.location.origin:"https://panel.example.com"} --node-id ${credential.nodeId}`:"";if(credential)return <Modal title="复制安装命令" kicker="服务器已创建" onClose={onClose}><div className="install-step"><span className="success-mark">1</span><div><b>在目标服务器执行</b><p>安装器会自动安装 Agent、nftables、tc 和 Realm。</p></div></div><div className="command-box"><code>{installCommand}</code><button onClick={()=>navigator.clipboard.writeText(installCommand)}>复制命令</button></div><div className="install-step"><span>2</span><div><b>按提示粘贴 Agent Token</b><p>Token 只显示这一次，不会写进命令历史。</p></div></div><div className="token-box"><code>{credential.token}</code><button className="outline wide" onClick={()=>navigator.clipboard.writeText(credential.token)}>复制 Agent Token</button></div><div className="modal-actions"><button className="primary" onClick={onClose}>完成</button></div></Modal>;return <Modal title={initial?"配置服务器":"接入服务器"} kicker={initial?"网络参数":"第一步"} onClose={onClose}><form className="form-grid" onSubmit={e=>{e.preventDefault();onSave(n)}}><label className="full">服务器名称<input required value={n.name} onChange={field("name")} placeholder="例如：广州入口、香港出口"/></label><label className="full">服务器用途<select value={n.role} onChange={field("role")}><option value="ingress">入口服务器</option><option value="egress">出口服务器</option><option value="both">入口 / 出口均可</option></select><small>用途只影响线路选择，不会自动修改服务器网络。</small></label><details className="advanced full" open={Boolean(initial)}><summary>网络参数（可稍后配置）</summary><div className="advanced-grid"><label>公网地址<input value={n.public_address} onChange={field("public_address")} placeholder="例如：203.0.113.18"/></label><label>内网地址<input value={n.private_address} onChange={field("private_address")} placeholder="例如：10.24.0.3"/></label><label>公网网卡<input value={n.public_interface} onChange={field("public_interface")}/></label><label>内网网卡<input value={n.private_interface} onChange={field("private_interface")}/></label></div></details><div className="modal-actions full"><button type="button" className="outline" onClick={onClose}>取消</button><button className="primary" disabled={busy}>{busy?"保存中…":initial?"保存配置":"创建并获取安装命令"}</button></div></form></Modal>}

function AgentUpdateModal({node,onClose}:{node:Node;onClose:()=>void}){
  const command="curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update-agent.sh | sudo bash";
  return <Modal title={`更新 ${node.name}`} kicker="Agent 免 Token 更新" onClose={onClose}><div className="install-step"><span className="success-mark">1</span><div><b>在这台服务器执行</b><p>脚本会读取并保留现有 Node ID、Agent Token 和控制端地址。</p></div></div><div className="command-box"><code>{command}</code><button onClick={()=>void copyText(command)}>复制更新命令</button></div><div className="install-step"><span>2</span><div><b>等待 Agent 重新上线</b><p>更新失败会恢复旧程序；成功后面板约 5–10 秒显示新版本。</p></div></div><div className="token-box"><span>当前版本</span><code>{node.agent_version||"未上报"}</code><p>更新不需要重新输入 Agent Token，也不会修改现有转发配置。</p></div><div className="modal-actions"><button className="primary" onClick={onClose}>完成</button></div></Modal>;
}

function LineModal({nodes,probes,initial,busy,onClose,onSave}:{nodes:Node[];probes:LinkProbe[];initial:Line|null;busy:boolean;onClose:()=>void;onSave:(l:typeof blankLine)=>void}){
  const firstIngress=nodes.find(n=>n.role!=="egress");
  const firstEgress=nodes.find(n=>n.role!=="ingress"&&n.id!==firstIngress?.id);
  const initialEgresses=initial?.egress_node_ids?.length?initial.egress_node_ids:(initial?.egress_node_id?[initial.egress_node_id]:[]);
  const [line,setLine]=useState(()=>initial?{
    name:initial.name,mode:initial.mode,ingress_node_id:initial.ingress_node_id,egress_node_id:initial.egress_node_id,
    egress_node_ids:initialEgresses,active_egress_node_id:initial.active_egress_node_id||initial.egress_node_id,
    failover_enabled:initial.failover_enabled,listen_address:initial.listen_address,relay_port_range:initial.relay_port_range||"",
    engine:initial.egress_engine||initial.engine,ingress_engine:initial.ingress_engine||initial.engine||"nftables",egress_engine:initial.egress_engine||initial.engine||"nftables",enabled:initial.enabled,
  }:{...blankLine,ingress_node_id:firstIngress?.id||"",egress_node_id:firstEgress?.id||"",egress_node_ids:firstEgress?[firstEgress.id]:[],active_egress_node_id:firstEgress?.id||""});
  const field=(key:keyof typeof line)=>(e:React.ChangeEvent<HTMLInputElement|HTMLSelectElement>)=>setLine({...line,[key]:e.target.value} as typeof line);
  const ingress=nodes.find(n=>n.id===line.ingress_node_id);
  const primary=nodes.find(n=>n.id===line.egress_node_id);
  const availableEgresses=nodes.filter(n=>n.role!=="ingress"&&(line.mode==="exit_only"||n.id!==line.ingress_node_id));
  const orderedEgresses=[line.egress_node_id,...line.egress_node_ids.filter(id=>id!==line.egress_node_id)].filter(Boolean);
  const setMode=(mode:Mode)=>setLine(current=>{
    const primaryID=current.egress_node_id||firstEgress?.id||"";
    return {...current,mode,ingress_node_id:mode==="exit_only"?primaryID:(current.ingress_node_id===primaryID?(firstIngress?.id||""):current.ingress_node_id),egress_node_ids:primaryID?[primaryID]:[],active_egress_node_id:primaryID,failover_enabled:false,listen_address:mode==="exit_only"?(nodes.find(n=>n.id===primaryID)?.private_address||""):"0.0.0.0",engine:current.egress_engine,ingress_engine:mode==="exit_only"?current.egress_engine:current.ingress_engine};
  });
  const setPrimary=(id:string)=>setLine(current=>{
    const node=nodes.find(n=>n.id===id);
    const ids=[id,...current.egress_node_ids.filter(value=>value!==id)].filter(Boolean);
    return {...current,egress_node_id:id,egress_node_ids:current.mode==="exit_only"?[id]:ids,active_egress_node_id:ids.includes(current.active_egress_node_id)?current.active_egress_node_id:id,ingress_node_id:current.mode==="exit_only"?id:current.ingress_node_id,listen_address:current.mode==="exit_only"?(node?.private_address||""):"0.0.0.0"};
  });
  const toggleBackup=(id:string)=>setLine(current=>{
    const selected=current.egress_node_ids.includes(id);
    const ids=selected?current.egress_node_ids.filter(value=>value!==id):[...current.egress_node_ids,id];
    const ordered=[current.egress_node_id,...ids.filter(value=>value!==current.egress_node_id)].filter(Boolean);
    return {...current,egress_node_ids:ordered,active_egress_node_id:ordered.includes(current.active_egress_node_id)?current.active_egress_node_id:current.egress_node_id,failover_enabled:ordered.length>1?current.failover_enabled:false};
  });
  const ready=Boolean(line.name&&primary&&(line.mode==="exit_only"?line.listen_address:ingress&&ingress.id!==primary.id)&&(!line.failover_enabled||orderedEgresses.length>1));
  return <Modal title={initial?"修改线路":"创建线路"} kicker={initial?"调整拓扑":"线路拓扑"} onClose={onClose}><form className="form-grid" onSubmit={e=>{e.preventDefault();const ids=line.mode==="exit_only"?[line.egress_node_id]:orderedEgresses;onSave({...line,engine:line.egress_engine,ingress_engine:line.mode==="exit_only"?line.egress_engine:line.ingress_engine,ingress_node_id:line.mode==="exit_only"?line.egress_node_id:line.ingress_node_id,egress_node_ids:ids,active_egress_node_id:ids.includes(line.active_egress_node_id)?line.active_egress_node_id:line.egress_node_id,failover_enabled:line.mode==="dual_managed"&&line.failover_enabled&&ids.length>1})}}>
    <div className="mode-picker full"><button type="button" className={line.mode==="dual_managed"?"selected":""} onClick={()=>setMode("dual_managed")}><b>双端托管</b><span>入口、出口都由面板管理</span><small>客户端 → 入口 Agent → 出口 Agent → 落地</small></button><button type="button" className={line.mode==="exit_only"?"selected":""} onClick={()=>setMode("exit_only")}><b>仅出口接管</b><span>第一跳已经自行配置</span><small>已有入口转发 → 出口 Agent → 落地</small></button></div>
    <label className="full">线路名称<input required value={line.name} onChange={field("name")} placeholder="例如：入口 A → 出口 B"/></label>
    {line.mode==="dual_managed"&&<label>入口服务器<select required value={line.ingress_node_id} onChange={e=>{const id=e.target.value;setLine(current=>({...current,ingress_node_id:id,egress_node_ids:current.egress_node_ids.filter(value=>value!==id),egress_node_id:current.egress_node_id===id?"":current.egress_node_id}))}}><option value="">请选择</option>{nodes.filter(n=>n.role!=="egress").map(n=><option key={n.id} value={n.id}>{n.name}{n.status!=="online"?"（离线）":""}</option>)}</select></label>}
    <label className={line.mode==="exit_only"?"full":""}>主出口服务器<select required value={line.egress_node_id} onChange={e=>setPrimary(e.target.value)}><option value="">请选择</option>{availableEgresses.map(n=><option key={n.id} value={n.id}>{n.name}{n.status!=="online"?"（离线）":""}</option>)}</select></label>
    {line.mode==="dual_managed"&&<div className="egress-options full"><div><b>备用出口（按顺序切换）</b><small>主出口不可用时，面板会切换入口转发目标；备用出口会提前下发同一中继端口。</small></div>{availableEgresses.filter(n=>n.id!==line.egress_node_id).map(n=>{const selected=line.egress_node_ids.includes(n.id),probe=probes.find(p=>p.ingress_node_id===line.ingress_node_id&&p.egress_node_id===n.id);return <label className="check-row" key={n.id}><input aria-label={`选择备用出口 ${n.name}`} type="checkbox" checked={selected} onChange={()=>toggleBackup(n.id)}/><span><b>{n.name}</b><small>{probe?.has_succeeded?`${probe.latency_ms.toFixed(1)} ms · 丢包 ${probe.packet_loss.toFixed(0)}%`:n.status==="online"?"等待入口 Agent 探测":"服务器离线"}</small></span></label>})}{availableEgresses.length<=1&&<p>还没有其他可用出口服务器。</p>}<label className="failover-toggle"><input aria-label="启用自动故障切换" type="checkbox" checked={line.failover_enabled} disabled={orderedEgresses.length<2} onChange={e=>setLine({...line,failover_enabled:e.target.checked})}/><span><b>启用自动故障切换</b><small>连续 3 次探测失败后切换，主线路连续 3 次恢复后回切；ICMP 从未成功时仅按 Agent 在线状态判断。</small></span></label></div>}
    {line.mode==="exit_only"&&<label className="full">出口内网接入 IP<input required value={line.listen_address} onChange={field("listen_address")} placeholder="已有入口规则转入的出口内网 IP"/></label>}
    <label className="full">所有出口共同可用的中继端口（NAT 可选）<input value={line.relay_port_range} onChange={field("relay_port_range")} placeholder="留空不限制；例如 20000-20999,25000"/><small>多个出口必须都能使用这里的端口；双端托管自动分配，仅出口接管校验接入端口。</small></label>
    {line.mode==="dual_managed"&&<label>入口引擎<select value={line.ingress_engine} onChange={field("ingress_engine")}><option value="nftables">nftables（内核转发）</option><option value="realm">Realm</option></select><small>入口公网端口 → 出口内网 IP 与中继端口</small></label>}
    <label className={line.mode==="exit_only"?"full":""}>出口引擎<select value={line.egress_engine} onChange={e=>setLine({...line,egress_engine:e.target.value as Engine,engine:e.target.value as Engine,ingress_engine:line.mode==="exit_only"?e.target.value as Engine:line.ingress_engine})}><option value="nftables">nftables（内核转发）</option><option value="realm">Realm</option></select><small>出口中继端口 → 每条规则的落地 IP 与端口</small></label>
    {initial&&<div className="change-warning full">保存后，线路下已有转发会迁移到新的入口和出口，并自动重新下发。</div>}
    <div className="route-summary full"><small>{initial?"修改后的路径":"保存后的路径"}</small><b>{line.mode==="dual_managed"?(ingress?.name||"入口服务器"):"已有入口"} → {primary?.name||"主出口"}{orderedEgresses.length>1?`（另有 ${orderedEgresses.length-1} 个备用）`:""} → 每条规则的落地地址</b><span>{line.mode==="dual_managed"?`入口 ${engineName(line.ingress_engine)} → 出口 ${engineName(line.egress_engine)}`:`出口 ${engineName(line.egress_engine)}`} · 出口端口：{line.relay_port_range||"不限制（自动从 30000 起分配）"}</span></div>
    <div className="modal-actions full"><button type="button" className="outline" onClick={onClose}>取消</button><button className="primary" disabled={busy||!ready}>{busy?"保存中…":initial?"保存并重新下发":"创建线路"}</button></div>
  </form></Modal>
}

type RuleDraft = {line_id:string;name:string;protocol:"tcp"|"udp"|"both";listen_port:number;target_host:string;target_port:number;upload_mbps:number;download_mbps:number;burst_kbytes:number;enabled:boolean};
function RuleModal({lines,nodes,initial,initialLine,busy,onNeedLine,onClose,onSave}:{lines:Line[];nodes:Node[];initial:Rule|null;initialLine:string;busy:boolean;onNeedLine:()=>void;onClose:()=>void;onSave:(r:RuleDraft)=>void}){const [draft,setDraft]=useState<RuleDraft>(()=>initial?{line_id:initial.line_id||initialLine||lines[0]?.id||"",name:initial.name,protocol:initial.protocol,listen_port:initial.listen_port,target_host:initial.target_host,target_port:initial.target_port,upload_mbps:initial.upload_mbps,download_mbps:initial.download_mbps,burst_kbytes:initial.burst_kbytes,enabled:initial.enabled}:{line_id:initialLine||lines[0]?.id||"",name:"",protocol:"both",listen_port:10000,target_host:"",target_port:0,upload_mbps:0,download_mbps:0,burst_kbytes:512,enabled:true});const field=(key:keyof RuleDraft)=>(e:React.ChangeEvent<HTMLInputElement|HTMLSelectElement>)=>setDraft({...draft,[key]:e.target.type==="number"?Number(e.target.value):e.target.value} as RuleDraft);const line=lines.find(l=>l.id===draft.line_id);const autoName=draft.name||`${draft.target_host||"落地"}:${draft.target_port||draft.listen_port}`;if(!lines.length)return <Modal title="先创建线路" kicker="缺少前置配置" onClose={onClose}><div className="empty compact"><span>1</span><h3>转发规则必须属于一条线路</h3><p>线路负责固定入口、出口、接管模式和两个阶段的转发引擎。</p><button className="primary" onClick={onNeedLine}>去创建线路</button></div></Modal>;return <Modal title={initial?"修改转发":"新建转发"} kicker={initial?"调整并重新下发":"日常操作"} onClose={onClose}><form className="form-grid" onSubmit={e=>{e.preventDefault();onSave({...draft,name:autoName,target_port:draft.target_port||draft.listen_port})}}><label className="full">使用线路<select required value={draft.line_id} onChange={field("line_id")}>{lines.map(l=><option key={l.id} value={l.id}>{l.name} · {modeName(l.mode)} · {lineEngineText(l)}</option>)}</select></label>{line&&<div className="selected-line full"><span className="line-badge">{modeName(line.mode)}</span><div><b>{line.mode==="exit_only"?"已有入口":nodeName(nodes,line.ingress_node_id)} → {nodeName(nodes,line.active_egress_node_id||line.egress_node_id)}</b><small>{line.mode==="dual_managed"?`面板下发入口与 ${line.egress_node_ids?.length||1} 个出口段`:"面板只下发出口段"} · 出口端口 {line.relay_port_range||"不限制"}</small></div><em>{lineEngineText(line)}</em></div>}<label className="full">规则名称（可选）<input value={draft.name} onChange={field("name")} placeholder="留空则按落地地址自动命名"/></label><label>协议<select value={draft.protocol} onChange={field("protocol")}><option value="both">TCP + UDP</option><option value="tcp">仅 TCP</option><option value="udp">仅 UDP</option></select></label><label>{line?.mode==="exit_only"?"出口接入端口":"入口公网端口"}<input required type="number" min="1" max="65535" value={draft.listen_port} onChange={field("listen_port")}/><small>{line?.mode==="exit_only"?(line.relay_port_range?`必须位于 ${line.relay_port_range}`:"出口端口未限制"):"入口端口可使用 1-65535"}</small></label><label>落地 IP<input required value={draft.target_host} onChange={field("target_host")} placeholder="例如：192.0.2.88"/></label><label>落地端口（可选）<input type="number" min="0" max="65535" value={draft.target_port||""} onChange={field("target_port")} placeholder={`默认 ${draft.listen_port}`}/></label><details className="advanced full" open={Boolean(initial&&(draft.upload_mbps||draft.download_mbps))}><summary>速率控制（可选）</summary><div className="advanced-grid"><label>上传限速（Mbps）<input type="number" min="0" value={draft.upload_mbps} onChange={field("upload_mbps")}/></label><label>下载限速（Mbps）<input type="number" min="0" value={draft.download_mbps} onChange={field("download_mbps")}/></label><label>突发流量（KB）<input type="number" min="32" value={draft.burst_kbytes} onChange={field("burst_kbytes")}/></label></div></details>{initial&&<div className="change-warning full">保存后会自动替换旧配置，并重新下发到对应入口和出口 Agent。</div>}<div className="modal-actions full"><button type="button" className="outline" onClick={onClose}>取消</button><button className="primary" disabled={busy||!line}>{busy?"保存中…":initial?"保存并重新下发":"创建并下发"}</button></div></form></Modal>}

function Modal({title,kicker="配置",onClose,children}:{title:string;kicker?:string;onClose:()=>void;children:React.ReactNode}){return <div className="modal-backdrop"><section className="modal" role="dialog" aria-modal="true" aria-label={title}><div className="modal-head"><div><p className="eyebrow">{kicker}</p><h2>{title}</h2></div><button className="icon-button" aria-label="关闭" onClick={onClose}>×</button></div>{children}</section></div>}
