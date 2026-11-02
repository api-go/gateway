'use strict';
let trashItems=[],trashBusy=false;
async function refreshTrash(){
 $('trashMessage').classList.add('hidden');
 trashItems=await api('trash');renderTrash();
}
function renderTrash(){
 $('trashRows').innerHTML=trashItems.map((item,index)=>`<div class="rule-summary"><div><strong>${esc(item.record.draft.domains.join(', '))}</strong><p class="muted small">${esc(new Date(item.deletedAt).toLocaleString())} · ${esc(item.deletedBy||'—')}</p><p class="muted small">${item.record.current?'删除前已发布 v'+esc(item.record.version):'仅草稿，尚未发布'}</p></div><div class="actions"><button type="button" data-restore-host="${index}">恢复</button><button type="button" class="danger" data-purge-host="${index}">永久删除</button></div></div>`).join('')||'<div class="empty"><p>回收站为空。</p></div>';
 for(const action of ['restore','purge']){
  $('trashRows').querySelectorAll(`[data-${action}-host]`).forEach(button=>button.onclick=()=>trashRun(async()=>{
   if(trashBusy)return;
   trashBusy=true;
   try{
    const item=trashItems[Number(button.dataset[action+'Host'])],record=item.record;
    const title=action==='restore'?'恢复 Host':'永久删除 Host';
    const effect=action==='restore'?(record.current?'恢复后立即重新启用删除前的已发布配置，未发布的草稿保持独立。':'恢复到 Host 列表，仍为未发布草稿。'):'将永久删除保留的草稿、发布配置和历史，无法从回收站恢复。';
    if(!await confirmAction(title,record.draft.domains.join(', '),effect,action==='restore'?'恢复':'永久删除'))return;
    await api(action,{host:{id:record.draft.id},revision:record.revision});
    await refreshTrash();await refreshHosts();
   }finally{trashBusy=false}
  }));
 }
}
async function trashRun(action){try{await action()}catch(error){$('trashMessage').textContent=error.message;$('trashMessage').classList.remove('hidden')}}
$('openTrash').onclick=()=>{if(!$('trashDialog').open)$('trashDialog').showModal();$('trashRows').textContent='正在读取…';trashRun(refreshTrash)};
$('refreshTrash').onclick=()=>{if(!trashBusy)trashRun(refreshTrash)};
$('closeTrash').onclick=$('doneTrash').onclick=()=>$('trashDialog').close();
