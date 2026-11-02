'use strict';
let aclRules=[];
const aclLegacy=rule=>!!(rule.action||(rule.conditions||[]).length||rule.script||rule.timeoutMS);
function aclConditionSummary(condition){const label=Object.fromEntries(sources)[condition.source]||condition.source;return `${label}${condition.name?' '+condition.name:''} ${condition.value||'存在'}`}
function aclSummary(rule){if(aclLegacy(rule))return '旧版 ACL（兼容执行）';return `${(rule.scope||[]).map(aclConditionSummary).join(' 且 ')||'全部请求'} · ${(rule.allow||[]).length?'允许 '+rule.allow.length+' 项条件':'全部拒绝'}`}
function modernACL(rules){
 const copy=clone(rules||[]);
 // Only convert the exact, equivalent allow-then-deny pair from the first demo.
 if(copy.length===2&&copy[0].action==='allow'&&copy[1].action==='deny'&&!copy[1].script&&copy[0].enabled===copy[1].enabled&&(copy[0].mode||'enforce')===(copy[1].mode||'enforce')){
  const [allow,deny]=copy,scope=deny.conditions||[],conditions=allow.conditions||[];
  if(scope.every((condition,index)=>JSON.stringify(condition)===JSON.stringify(conditions[index]))){
   const converted=clone(scope).map(condition=>{const match=condition.source==='path'&&condition.operator==='regex'&&/^\^\/[a-zA-Z0-9_\/-]+\(\/\|\$\)$/.test(condition.value);return match?{...condition,operator:'directory',value:condition.value.slice(1,-5)}:condition});
   return [{id:deny.id,enabled:deny.enabled,mode:deny.mode||'enforce',scope:converted,allow:[{conditions:conditions.slice(scope.length),script:allow.script||'',timeoutMS:allow.timeoutMS||10}]}];
  }
 }
 return copy;
}
function fillACL(host){aclRules=modernACL(host.acl);renderACL()}
function simpleIPGrant(grant){return !grant.script&&(grant.conditions||[]).length===1&&grant.conditions[0].source==='ip'&&['equals','cidr'].includes(grant.conditions[0].operator)}
function aclConditionRow(condition,index,kind,grantIndex,conditionIndex){return `<div class="condition-row"><select data-acl-condition="${conditionIndex}" data-rule="${index}" data-kind="${kind}" data-grant="${grantIndex}" data-field="source">${options(sources,condition.source)}</select><input data-acl-condition="${conditionIndex}" data-rule="${index}" data-kind="${kind}" data-grant="${grantIndex}" data-field="name" value="${esc(condition.name)}" placeholder="Header / Cookie 名称"><select data-acl-condition="${conditionIndex}" data-rule="${index}" data-kind="${kind}" data-grant="${grantIndex}" data-field="operator">${options([...operators,['directory','目录前缀']],condition.operator)}</select><input data-acl-condition="${conditionIndex}" data-rule="${index}" data-kind="${kind}" data-grant="${grantIndex}" data-field="value" value="${esc(condition.value)}" placeholder="匹配值"><button type="button" data-acl-delete-condition="${conditionIndex}" data-rule="${index}" data-kind="${kind}" data-grant="${grantIndex}" aria-label="移除条件">✕</button></div>`}
function updateACLTitles(){document.querySelectorAll('[data-acl-title]').forEach(title=>title.textContent=aclSummary(ruleDraftRows[title.dataset.aclTitle]))}
function renderACLForm(){
 const rows=$('ruleFields');
 rows.innerHTML=ruleDraftRows.map((rule,index)=>{
  const header=`<div class="limit-head"><strong data-acl-title="${index}">${esc(aclSummary(rule))}</strong><label class="check"><input type="checkbox" data-acl="${index}" data-field="enabled" ${rule.enabled?'checked':''}>启用</label></div>`;
  if(aclLegacy(rule))return `<div class="limit-card">${header}<p class="muted small">保留原执行语义。新建策略使用保护范围和放行条件；这项旧版配置可在 API 中管理。</p></div>`;
  return `<div class="limit-card">${header}<p class="rule-label">保护范围 <span class="muted small">全部满足；为空表示整个 Host</span></p>${(rule.scope||[]).map((condition,ci)=>aclConditionRow(condition,index,'scope',-1,ci)).join('')}<button type="button" data-acl-add-condition="${index}" data-kind="scope" class="add-small">＋ 添加范围条件</button><p class="rule-label">允许访问的 IP / 网段</p><textarea data-acl-ips="${index}" rows="3" placeholder="每行一个 IP 或 CIDR，例如 192.168.10.0/24">${esc(rule.ipText??(rule.allow||[]).filter(simpleIPGrant).map(grant=>grant.conditions[0].value).join('\n'))}</textarea><p class="muted small">满足任意一个放行条件即可。范围内其他请求默认拒绝；没有放行条件就是全部拒绝。</p><details ${(rule.allow||[]).some(grant=>!simpleIPGrant(grant))?'open':''}><summary>其他放行条件与低代码</summary>${(rule.allow||[]).map((grant,gi)=>simpleIPGrant(grant)?'':`<div class="limit-card"><div class="limit-head"><strong>或：满足以下条件</strong><button type="button" data-acl-remove-grant="${gi}" data-rule="${index}" aria-label="移除放行条件">✕</button></div>${(grant.conditions||[]).map((condition,ci)=>aclConditionRow(condition,index,'allow',gi,ci)).join('')}<button type="button" data-acl-add-condition="${index}" data-kind="allow" data-grant="${gi}" class="add-small">＋ 添加同时满足的条件</button><label>低代码（可选，与本项条件同时满足）<textarea data-acl-script="${gi}" data-rule="${index}" spellcheck="false" placeholder="function(request) { return request.method === 'GET'; }">${esc(grant.script||'')}</textarea></label><label>超时（毫秒，默认 10，最大 100）<input type="number" min="1" max="100" data-acl-timeout="${gi}" data-rule="${index}" value="${grant.timeoutMS||10}"></label></div>`).join('')}<button type="button" data-acl-add-grant="${index}" class="add-small">＋ 添加另一种放行方式</button><p class="muted small">不同放行方式是 OR，每种方式内部条件是 AND。脚本 true 放行；false、异常、超时及非布尔结果不放行。request 包含 host、path、method、ip、ua、session、headers（标准名称 → 字符串数组）、cookies。不读取请求体。</p></details><details><summary>观察模式</summary><label>执行方式<select data-acl="${index}" data-field="mode">${options([['enforce','执行访问控制'],['observe','只观察并记录']],rule.mode||'enforce')}</select></label></details></div>`;
 }).join('')||'<div class="empty"><p>没有配置保护范围，默认允许。</p></div>';
 rows.querySelectorAll('[data-acl]').forEach(input=>input.onchange=()=>{ruleDraftRows[input.dataset.acl][input.dataset.field]=input.type==='checkbox'?input.checked:input.value;updateACLTitles()});
 const conditionList=element=>element.dataset.kind==='scope'?(ruleDraftRows[element.dataset.rule].scope??=[]):(ruleDraftRows[element.dataset.rule].allow[element.dataset.grant].conditions??=[]);
 rows.querySelectorAll('[data-acl-condition]').forEach(input=>input.onchange=()=>{conditionList(input)[input.dataset.aclCondition][input.dataset.field]=input.value;updateACLTitles()});
 rows.querySelectorAll('[data-acl-add-condition]').forEach(button=>button.onclick=()=>{const rule=ruleDraftRows[button.dataset.aclAddCondition];(button.dataset.kind==='scope'?(rule.scope??=[]):(rule.allow[button.dataset.grant].conditions??=[])).push({source:'path',operator:'prefix',name:'',value:''});renderACLForm()});
 rows.querySelectorAll('[data-acl-delete-condition]').forEach(button=>button.onclick=()=>{conditionList(button).splice(Number(button.dataset.aclDeleteCondition),1);renderACLForm()});
 rows.querySelectorAll('[data-acl-ips]').forEach(input=>input.oninput=()=>{ruleDraftRows[input.dataset.aclIps].ipText=input.value});
 rows.querySelectorAll('[data-acl-script]').forEach(input=>input.onchange=()=>{ruleDraftRows[input.dataset.rule].allow[input.dataset.aclScript].script=input.value});
 rows.querySelectorAll('[data-acl-timeout]').forEach(input=>input.onchange=()=>{ruleDraftRows[input.dataset.rule].allow[input.dataset.aclTimeout].timeoutMS=Number(input.value)});
 rows.querySelectorAll('[data-acl-add-grant]').forEach(button=>button.onclick=()=>{(ruleDraftRows[button.dataset.aclAddGrant].allow??=[]).push({conditions:[{source:'method',operator:'equals',value:'GET'}]});renderACLForm()});
 rows.querySelectorAll('[data-acl-remove-grant]').forEach(button=>button.onclick=()=>{ruleDraftRows[button.dataset.rule].allow.splice(Number(button.dataset.aclRemoveGrant),1);renderACLForm()});
}

function renderACL(){renderRuleList('acl',aclRules,'aclRows',aclSummary)}
$('addACL').onclick=()=>openRuleEditor('acl');
