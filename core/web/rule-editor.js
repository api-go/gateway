'use strict';
let ruleDraftRows=[],ruleEditing=null;
const ruleKinds={
 acl:{title:'ACL 规则',items:()=>aclRules,render:renderACL,form:renderACLForm,create:()=>({id:newID(),enabled:true,scope:[{source:'path',operator:'prefix',value:''}],allow:[]})},
 route:{title:'路由规则',items:()=>routes,render:renderRoutes,form:renderRouteForm,create:()=>({type:routeTab,path:routeTab==='static'?'/':'/*',to:'',authLevel:0})},
 limit:{title:'限流规则',items:()=>limits,render:renderLimits,form:renderLimitForm,create:()=>({id:newID(),enabled:true,requests:100,periodSeconds:60,scope:'node',mode:'enforce',missing:'shared',conditions:[],groupBy:[]})},
 affinity:{title:'会话保持规则',items:()=>affinityRules,render:renderAffinityRules,form:renderAffinityForm,create:()=>({id:newID(),path:'/*',methods:[],affinity:{mode:'auto',source:'header',name:'sessionid',missing:'balance',transform:'full',ttlSeconds:3600}})}
};
function routeSummary(rule){return `${rule.path} → ${rule.to}`}
function affinitySummary(rule){const a=rule.affinity||{};return `${rule.path||'/*'} · ${(rule.methods||[]).join(', ')||'全部方法'} · ${({off:'不保持',auto:'自动保持',redis:'自动保持',hash:'哈希保持',manual:'手动映射'})[a.mode]||a.mode||'不保持'}`}
function renderRuleList(kind,items,target,summary,include=()=>true){
 const rows=$(target);
 rows.innerHTML=items.map((rule,index)=>!include(rule)?'':`<div class="rule-summary"><div><strong>${esc(summary(rule))}</strong>${'enabled' in rule?`<span class="badge ${rule.enabled?'good':''}">${rule.enabled?'启用':'停用'}</span>`:''}${rule.mode==='observe'?'<span class="badge">只观察</span>':''}${kind==='acl'&&!aclLegacy(rule)?`<p class="muted small">${esc((rule.allow||[]).map(grant=>simpleIPGrant(grant)?grant.conditions[0].value:grant.script?'低代码放行条件':(grant.conditions||[]).map(aclConditionSummary).join(' 且 ')||'全部允许').join(' 或 ')||'范围内全部拒绝')}</p>`:''}</div><div class="actions"><button type="button" data-edit-rule="${index}">编辑</button><button type="button" class="danger" data-delete-rule="${index}">删除规则</button></div></div>`).join('')||'<div class="empty"><p>尚无规则，点击添加开始配置。</p></div>';
 rows.querySelectorAll('[data-edit-rule]').forEach(button=>button.onclick=()=>openRuleEditor(kind,Number(button.dataset.editRule)));
 rows.querySelectorAll('[data-delete-rule]').forEach(button=>button.onclick=()=>run(async()=>{
  const index=Number(button.dataset.deleteRule),rule=items[index];
  if(!await confirmAction('删除'+ruleKinds[kind].title,summary(rule),'仅移除这条规则，不删除 Host。保存草稿并发布后生效。','删除规则'))return;
  items.splice(index,1);ruleKinds[kind].render();
 }));
}
function openRuleEditor(kind,index=-1){
 const spec=ruleKinds[kind];
 ruleEditing={kind,index};
 ruleDraftRows=[index<0?spec.create():clone(spec.items()[index])];
 $('ruleEditorTitle').textContent=(index<0?'添加':'编辑')+(kind==='route'?Object.fromEntries(routeTypes)[ruleDraftRows[0].type]:spec.title);
 $('ruleEditorError').classList.add('hidden');
 spec.form();$('ruleEditor').showModal();
}
function closeRuleEditor(){$('ruleEditor').close();ruleEditing=null;ruleDraftRows=[];$('ruleFields').replaceChildren()}
function validateRuleDraft(kind,rule){
 if(kind==='acl'&&!aclLegacy(rule)){
  if(rule.ipText!==undefined){
   const ips=[...new Set(rule.ipText.split(/[\s,，]+/).filter(Boolean))];
   rule.allow=[...ips.map(value=>({conditions:[{source:'ip',operator:value.includes('/')?'cidr':'equals',value}]})),...(rule.allow||[]).filter(grant=>!simpleIPGrant(grant))];
   delete rule.ipText;
  }
  for(const condition of [...rule.scope||[],...(rule.allow||[]).flatMap(grant=>grant.conditions||[])]){
   if(condition.operator!=='exists'&&!condition.value?.trim())throw Error('请填写条件匹配值；如需保护整个 Host，请移除范围条件。');
  }
 }
 if(kind==='route'&&(!rule.path.trim()||!rule.to.trim()))throw Error('请填写匹配路径和目标。');
 if(kind==='affinity'){
  if(!rule.path.trim())throw Error('请填写匹配路径。');
  if(rule.mappingText!==undefined){
   let mappings;try{mappings=JSON.parse(rule.mappingText)}catch{throw Error('手动映射需要合法 JSON 数组。')}
   if(!Array.isArray(mappings))throw Error('手动映射必须是数组。');
   rule.affinity.mappings=mappings;delete rule.mappingText;
  }
 }
}
$('ruleEditorForm').onsubmit=event=>{
 event.preventDefault();
 try{
  const {kind,index}=ruleEditing,spec=ruleKinds[kind],rule=clone(ruleDraftRows[0]);
  validateRuleDraft(kind,rule);
  if(index<0)spec.items().push(rule);else spec.items()[index]=rule;
  spec.render();closeRuleEditor();
 }catch(error){$('ruleEditorError').textContent=error.message;$('ruleEditorError').classList.remove('hidden')}
};
$('closeRuleEditor').onclick=closeRuleEditor;
$('cancelRuleEditor').onclick=closeRuleEditor;
$('ruleEditor').oncancel=event=>{event.preventDefault();closeRuleEditor()};
