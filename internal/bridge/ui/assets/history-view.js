'use strict';
((root,factory) => {
  const exports = factory();
  if (typeof module !== 'undefined' && module.exports) module.exports = exports;
  else root.DriveBridgeHistory = exports;
})(typeof window === 'undefined' ? globalThis : window,() => {
  const advice={git_unavailable:'历史组件无法启动，请修复或更新应用',history_integrity:'历史校验失败，请保留原数据并检查日志',history_unavailable:'历史服务暂不可用，请稍后重试并检查日志',history_timeout:'历史任务超时，请稍后重试',identity_changed:'账号身份不一致，请核对账号绑定'};
  const labels = {discovered:'发现文件',local_saved:'本地保存',materialized:'取得正文',renamed:'改名或移动',deleted:'删除记录',remote_observed:'观察云端版本',upload_verified:'云端确认',restored:'恢复历史',conflict_opened:'需要合并',conflict_resolved:'合并已接受',draft_saved:'保存合并草稿',state_updated:'状态更新'};
  function defaultRole(v){if(v.event.kind==='draft_saved'&&v.bodies.draft)return 'draft';if(v.event.kind==='remote_observed'&&v.bodies.remote)return 'remote';return v.bodies.local?'local':v.bodies.remote?'remote':v.bodies.draft?'draft':'';}
  function restoreRequest(detail,requestID) {
    const v=detail?.version,c=detail?.current;
    if (!detail?.restore_available || !v || !c || c.entry.id !== v.event.entry.id || !c.parent?.id) throw new Error('当前目标已变化或不可恢复，请刷新后重新核对。');
    return {profile_id:v.event.profile_id,mount_id:v.event.mount_id,entry_id:c.entry.id,version_id:v.id,role:defaultRole(v),request_id:requestID,expected_generation:c.entry.generation,expected_path:c.entry.remote,expected_deleted:c.deleted,parent_id:c.parent.id,parent_generation:c.parent.generation};
  }
  function create({document,api,onMessage=()=>{},confirm=()=>true,randomID=()=>globalThis.crypto.randomUUID()}) {
    const $=id=>document.getElementById(id),make=(tag,text,className)=>{const e=document.createElement(tag);if(text!==undefined)e.textContent=text;if(className)e.className=className;return e;};
    let cursor='',generation=0,detailGeneration=0,selected=null,pendingRestore=null,loading=false,config=null;
    const notice=(text,error=false)=>{$('history-notice').textContent=text;$('history-notice').classList.toggle('error',error);$('history-notice').hidden=!text;};
    const params=values=>new URLSearchParams(Object.entries(values).filter(([,v])=>v!==''&&v!==undefined)).toString();
    const profile=id=>config?.profiles?.find(p=>p.id===id)?.name || id;
    async function load(append=false) {
      const ticket=++generation;if(!append){detailGeneration++;selected=null;pendingRestore=null;$('history-detail').hidden=true;}loading=true;$('history-load-more').disabled=true;
      try {
        if(!config){config=await api('/api/config');for(const p of config.profiles||[]){const option=make('option',p.name);option.value=p.id;$('history-profile').append(option);}}
        const route='/api/history?'+params({profile_id:$('history-profile').value,search:$('history-search').value.trim(),cursor:append?cursor:'',limit:50});
        const local=await api('/api/history/pending?'+params({profile_id:$('history-profile').value,search:$('history-search').value.trim()}));if(ticket!==generation)return;
        const page=await api(route);if(ticket!==generation)return;
        $('history-pending-rows').replaceChildren();const archived=new Set((page.versions||[]).map(v=>v.id));for(const v of local.versions||[]){if(archived.has(v.id))continue;const row=make('tr'),name=make('td');name.append(make('span',v.event.entry.remote,'mount-name'),make('small',profile(v.event.profile_id),'mount-path'));row.append(name,make('td','本地已保存 · 归档中'),make('td',new Date(v.event.at).toLocaleString()));const cell=make('td'),open=make('button','查看版本','button small');open.addEventListener('click',()=>openVersion(v));cell.append(open);row.append(cell);$('history-pending-rows').append(row);}$('history-pending-panel').hidden=!$('history-pending-rows').children.length;
        if(!append)$('history-rows').replaceChildren();
        for(const v of page.versions||[]){const tr=make('tr'),name=make('td');name.append(make('span',v.event.entry.remote,'mount-name'),make('small',profile(v.event.profile_id),'mount-path'));tr.append(name,make('td',labels[v.event.kind]||'文件变化'),make('td',new Date(v.recorded_at).toLocaleString()));const action=make('td'),open=make('button','查看版本','button small');open.addEventListener('click',()=>openVersion(v));action.append(open);tr.append(action);$('history-rows').append(tr);}
        cursor=page.cursor||'';$('history-load-more').hidden=!cursor;$('history-empty').hidden=Boolean($('history-rows').children.length);
        if(page.warnings?.length)notice('部分账号的历史不可用：'+page.warnings.map(w=>profile(w.profile_id)+'（'+w.code+'）').join('、')+'。本地文件仍保留。',true);else if(!append)notice('');
        await status(); await loadPolicy();
      }catch(error){if(ticket===generation)notice(error.message,true);}finally{if(ticket===generation){loading=false;$('history-load-more').disabled=false;}}
    }
    async function status(){try{const states=await api('/api/history/status');let pending=0;const errors=[];let overBudget=false;for(const s of Object.values(states)){pending+=Number(s.pending||0);overBudget=overBudget||Boolean(s.over_budget);if(s.last_error)errors.push(advice[s.last_error]||s.last_error);if(s.maintenance_error)errors.push('后台整理暂未完成：'+(advice[s.maintenance_error]||s.maintenance_error));}$('history-state').textContent=overBudget?'历史占用已超过软预算；受保护的版本仍保留，可调整保留策略或释放其他空间。':errors.length?'历史归档异常：'+[...new Set(errors)].join('、')+'；本机保存和云端同步分别运行。':pending?'本地已保存，'+pending+' 条记录正在后台归档。':'历史归档就绪；云端确认请查看“文件与同步”。';}catch(_){$('history-state').textContent='暂时无法查询历史归档状态，已保存的本地内容仍保留。';}}
    async function openVersion(version){
      const ticket=++detailGeneration;pendingRestore=null;selected=null;$('history-detail').hidden=false;$('history-body').textContent='正在读取历史正文…';$('history-diff').textContent='';$('history-media').replaceChildren();$('history-restore').disabled=true;
      try{
        const p=version.event.profile_id;const detail=await api('/api/history/versions/'+version.id+'?'+params({profile_id:p}));if(ticket!==detailGeneration)return;selected=detail;$('history-pin').disabled=Boolean(detail.version.pending);$('history-pin').textContent=detail.version.pinned?'取消固定保留':'固定保留此版本';
        const v=detail.version;$('history-file-name').textContent=v.event.entry.remote;$('history-version-info').textContent=(labels[v.event.kind]||'文件变化')+' · '+new Date(v.recorded_at).toLocaleString()+' · '+profile(p);
        const role=defaultRole(v);
        $('history-restore').disabled=!detail.restore_available||!role;
        $('history-target-info').textContent=detail.warning==='mount_archived'?'挂载已移除，历史只读保留。':!detail.current?'当前目标不可用，不能恢复。':detail.current.deleted?'恢复会重新创建云端文件，可能产生新的链接。':'恢复会成为一次新的本地修改，随后由后台同步。';
        if(!role){$('history-body').textContent='此记录只有目录或文件元数据，未取得正文；不代表文件为空。';return;}
        const content=await api('/api/history/body?'+params({profile_id:p,version_id:v.id,role}));if(ticket!==detailGeneration)return;$('history-body').textContent=content.text?content.content:'二进制文件（'+content.size+' 字节），不显示文本差异。';
        let related;try{related=await api('/api/history?'+params({profile_id:p,entry_id:v.event.entry.id,limit:100}));}catch(_){related={versions:[]};}const local=await api('/api/history/pending?'+params({profile_id:p,entry_id:v.event.entry.id}));if(ticket!==detailGeneration)return;const all=new Map();for(const item of [...(related.versions||[]),...(local.versions||[])])all.set(item.id,item);if(!all.has(v.id))all.set(v.id,v);related.versions=[...all.values()].sort((a,b)=>new Date(b.event.at)-new Date(a.event.at));
        $('history-compare-left').replaceChildren();$('history-compare-right').replaceChildren();
        const available=(related.versions||[]).filter(x=>defaultRole(x));
        for(const item of available){for(const id of ['history-compare-left','history-compare-right']){const option=make('option',new Date(item.recorded_at).toLocaleString()+' · '+(labels[item.event.kind]||'文件变化'));option.value=item.id;$(id).append(option);}}
        $('history-compare-right').value=v.id;const index=available.findIndex(x=>x.id===v.id);$('history-compare-left').value=available[index+1]?.id||v.id;
        for(const [key] of Object.entries(v.media||{}).filter(([key])=>key.startsWith(role+':')).slice(0,24)){const figure=make('figure'),img=make('img');img.alt='此历史版本的图片';img.loading='lazy';img.src='/api/history/body?'+params({profile_id:p,version_id:v.id,role:'media:'+key});figure.append(img);$('history-media').append(figure);}
        if(available.length>1){$('history-compare').disabled=false;await compare(ticket);}else{$('history-compare').disabled=true;$('history-diff').textContent='目前只有一个可比较的正文版本。';}
      }catch(error){if(ticket===detailGeneration)notice(error.message,true);}
    }
    async function compare(ticket=detailGeneration){if(!selected)return;try{const diff=await api('/api/history/diff?'+params({profile_id:selected.version.event.profile_id,left:$('history-compare-left').value,right:$('history-compare-right').value}));if(ticket!==detailGeneration)return;$('history-diff').textContent=diff.binary?'二进制版本：可以恢复，暂不提供逐行差异。':diff.limited?'差异超出显示限制或计算超时。正文保留完整，不能把此视图当作完整合并结果。\n'+(diff.patch||''):diff.patch||'两个版本的正文相同。';}catch(error){notice(error.message,true);}}
    async function restore(){
      if(!selected)return;
      try{
        if(!pendingRestore){if(!confirm('将此版本恢复为当前文件的新修改？现有已保存内容保留在历史中，恢复后会后台同步。'))return;pendingRestore=restoreRequest(selected,randomID());}
        $('history-restore').disabled=true;const result=await api('/api/history/restore','POST',pendingRestore);
        if(result.local_saved){pendingRestore=null;notice('历史版本已在本机恢复，正在等待后台同步。');onMessage('历史版本已在本机恢复，云端状态请查看文件与同步。');await load();}
      }catch(error){notice(error.message,true);if(error.status===409)pendingRestore=null;}finally{$('history-restore').disabled=!selected?.restore_available;}
    }
    async function loadPolicy(){const p=$('history-profile').value;$('history-policy-save').disabled=!p;if(!p)return;try{const policy=await api('/api/history/policy?'+params({profile_id:p}));if($('history-profile').value!==p)return;$('history-keep-days').value=policy.keep_days;$('history-minimum').value=policy.minimum_versions;$('history-budget').value=(policy.max_bytes/(1024**3)).toFixed(3);}catch(error){notice(error.message,true);}}
    $('history-policy-form').addEventListener('submit',async event=>{event.preventDefault();if(!$('history-policy-form').reportValidity())return;const profile_id=$('history-profile').value;if(!profile_id)return;try{await api('/api/history/policy','POST',{profile_id,policy:{keep_days:Number($('history-keep-days').value),minimum_versions:Number($('history-minimum').value),max_bytes:Math.ceil(Number($('history-budget').value)*1024**3)}});notice('历史保留设置已保存，后台整理时生效。');}catch(error){notice(error.message,true);}});
    $('history-pin').addEventListener('click',async()=>{if(!selected)return;const v=selected.version;try{const result=await api('/api/history/pin','POST',{profile_id:v.event.profile_id,version_id:v.id,pinned:!v.pinned});v.pinned=result.pinned;$('history-pin').textContent=v.pinned?'取消固定保留':'固定保留此版本';notice(v.pinned?'此版本已固定保留。':'已取消固定，将按保留策略处理。');}catch(error){notice(error.message,true);}});
    $('history-refresh').addEventListener('click',()=>load());$('history-load-more').addEventListener('click',()=>{if(!loading)load(true);});$('history-profile').addEventListener('change',()=>load());
    let searchTimer;$('history-search').addEventListener('input',()=>{clearTimeout(searchTimer);searchTimer=setTimeout(()=>load(),300);});
    $('history-compare').addEventListener('click',()=>compare());$('history-restore').addEventListener('click',restore);
    return {load,status};
  }
  return {create,restoreRequest,defaultRole,labels};
});
