'use strict';
(function (root, factory) {
  const rules = factory();
  if (typeof module === 'object' && module.exports) module.exports = rules;
  else root.DriveBridgeRules = rules;
})(typeof window === 'object' ? window : globalThis, function () {
  const defaultIDs = new Set(['simple-break-paragraph','simple-break-table']);
  const restartMessage = '重启 DriveBridge 后生效；当前同步继续使用现有规则';
  function create({document,api,confirm=()=>false}) {
    const $ = id => document.getElementById(id);
    const make = (tag,text) => { const element=document.createElement(tag); if(text!==undefined) element.textContent=text; return element; };
    let draft=null, revision='', loaded=false, flight=null, busy=false, dirty=false, serial=0;
    const controls=[];
    function notice(text,error=false) { $('conversion-rules-status').textContent=text; $('conversion-rules-status').classList.toggle('error',error); }
    function preview() { $('conversion-rules-preview').textContent=draft ? JSON.stringify(draft,null,2) : ''; }
    function changed() { dirty=true; preview(); notice('有未保存的规则。保存后需重启 DriveBridge 才会生效。'); }
    function availability() {
      $('conversion-rules-reload').disabled=busy||Boolean(flight);
      $('conversion-rules-add').disabled=!draft||busy;
      $('conversion-rules-save').disabled=!draft||busy;
      for(const control of controls) control.element.disabled=busy||control.locked();
    }
    function field(row,element,label,locked=()=>false) {
      element.setAttribute('aria-label',label); const cell=make('td'); cell.append(element); row.append(cell);
      controls.push({element,locked}); return element;
    }
    function select(values,value) {
      const element=make('select');
      for(const [key,label] of values) { const option=make('option',label); option.value=key; element.append(option); }
      element.value=value; return element;
    }
    function render() {
      controls.length=0; $('conversion-rules-rows').replaceChildren();
      for(const rule of draft.rules) {
        const row=make('tr'), fixed=defaultIDs.has(rule.id);
        const source=field(row,make('input'),'原始字符串',()=>fixed); source.value=rule.source; source.maxLength=65536;
        const mode=field(row,select([['text','文本'],['newline','换行'],['delete','删除']],rule.target==='\n'?'newline':rule.target===''?'delete':'text'),'替换目标类型');
        const target=field(row,make('input'),'替换文本',()=>mode.value!=='text'); target.value=mode.value==='text'?rule.target:''; target.maxLength=65536;
        const context=field(row,select([['paragraph','段落'],['table','表格']],rule.context),'作用位置',()=>fixed);
        const match=field(row,select([['literal','普通字符串'],['exact','完整 HTML 标签'],['simple_html_break','HTML 换行标签']],rule.match),'匹配方式',()=>fixed);
        const remove=field(row,make('button','删除'),'删除此规则',()=>fixed); remove.type='button'; remove.className='button small';
        if(fixed) { source.title=context.title=match.title='内置规则的匹配边界固定；可以修改替换目标。'; remove.title='内置规则不能删除；可将目标设为“删除”以移除匹配内容。'; }
        const newline=mode.children[1]; newline.disabled=rule.match==='literal';
        source.addEventListener('input',()=>{rule.source=source.value; changed();});
        target.addEventListener('input',()=>{if(mode.value==='text') rule.target=target.value; changed();});
        mode.addEventListener('change',()=>{
          if(mode.value==='newline'&&rule.match==='literal') { mode.value='text'; }
          rule.target=mode.value==='newline'?'\n':mode.value==='delete'?'':target.value;
          availability(); changed();
        });
        context.addEventListener('change',()=>{rule.context=context.value;changed();});
        match.addEventListener('change',()=>{
          rule.match=match.value; newline.disabled=rule.match==='literal';
          if(rule.match==='literal'&&mode.value==='newline') {mode.value='text';rule.target=target.value;}
          availability();changed();
        });
        remove.addEventListener('click',()=>{draft.rules=draft.rules.filter(item=>item!==rule); render(); changed();});
        $('conversion-rules-rows').append(row);
      }
      preview();availability();
    }
    async function load() {
      if(loaded) return;
      if(flight) return flight;
      notice('正在读取替换规则…');
      flight=(async()=>{
        try {
          const view=await api('/api/conversion-rules');
          draft=JSON.parse(JSON.stringify(view.document)); revision=view.revision; loaded=true; dirty=false;
          $('conversion-rules-path').textContent=view.path;render();
          notice(view.restart_required?restartMessage:'规则保存在本机；保存后重启 DriveBridge 生效。');
        } catch(error) { notice(error.code==='unavailable'?'此管理入口暂不支持编辑替换规则。':error.message,true); }
        finally { flight=null;availability(); }
      })();
      return flight;
    }
    async function save() {
      if(!draft||busy) return;
      busy=true;availability();notice('正在保存规则…');
      try {
        const view=await api('/api/conversion-rules','PUT',{revision,document:JSON.parse(JSON.stringify(draft))});
        revision=view.revision; draft=JSON.parse(JSON.stringify(view.document)); $('conversion-rules-path').textContent=view.path;
        dirty=false;render();notice(restartMessage);
      } catch(error) { notice(`${error.message || '保存未完成。'} 未保存的编辑仍保留在此页。`,true); }
      finally {busy=false;availability();}
    }
    $('conversion-rules-add').addEventListener('click',()=>{
      if(!draft||busy) return;
      let id; do {id=`custom-${Date.now().toString(36)}-${++serial}`;} while(draft.rules.some(rule=>rule.id===id));
      draft.rules.push({id,source:'',target:'',context:'paragraph',match:'literal'}); render();changed();
    });
    $('conversion-rules-save').addEventListener('click',save);
    $('conversion-rules-reload').addEventListener('click',async()=>{
      if(busy||flight) return;
      if(dirty&&!confirm('重新读取将放弃此页尚未保存的替换规则，是否继续？')) return;
      loaded=false; await load();
    });
    availability();
    return {load};
  }
  return {create};
});
