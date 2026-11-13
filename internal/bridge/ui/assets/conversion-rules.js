'use strict';
(function (root, factory) {
  const rules = factory();
  if (typeof module === 'object' && module.exports) module.exports = rules;
  else root.DriveBridgeRules = rules;
})(typeof window === 'object' ? window : globalThis, function () {
  const policies = {
    'yaml-front-matter': {mode:'保留 YAML 元数据',target:'YAML 代码块（无损保留）',context:'文档开头',match:'YAML 元数据区块'},
    'plain-table-resize': {mode:'保留表格单元格',target:'保留单元格内容与安全布局',context:'文档结构',match:'普通表格增删行列'},
    'basic-inline-html': {source:'<strong>…</strong> / <b>…</b> / <em>…</em> / <i>…</i>',mode:'保留粗体与斜体',target:'完整配对的简单标签转为原生粗体或斜体',context:'段落、标题及表格等行内内容',match:'无属性、完整配对的基本格式标签'},
    'simple-html-paragraph': {source:'<p>…</p> / <div>…</div>',mode:'保留段落边界',target:'单个简单 HTML 容器转为原生段落',context:'独立 HTML 段落',match:'单个无属性容器及安全行内内容'},
    'indented-code': {source:'四空格或制表符缩进的代码',mode:'保留代码原文',target:'缩进代码转为原生代码块，正文不替换',context:'Markdown 代码区块',match:'缩进代码 AST 节点'},
    'ordinary-dollar-text': {source:'$100 / $20.50 / $HOME / $PATH',mode:'保留普通文字',target:'保留明确的金额及大写变量；公式继续保护',context:'普通文字节点',match:'明确金额或大写变量，排除公式分隔符'}
  };
  const defaultIDs = new Set(['simple-break-paragraph','simple-break-table',...Object.keys(policies)]);
  const restartMessage = '重启 DriveBridge 后生效；当前同步继续使用现有规则';
  function create({document,api,confirm=()=>false}) {
    const $ = id => document.getElementById(id);
    const make = (tag,text) => { const element=document.createElement(tag); if(text!==undefined) element.textContent=text; return element; };
    let draft=null, revision='', loaded=false, flight=null, busy=false, loading=false, dirty=false, serial=0;
    const controls=[], modes=new Map();
    function notice(text,error=false) { $('conversion-rules-status').textContent=text; $('conversion-rules-status').classList.toggle('error',error); }
    function preview() { $('conversion-rules-preview').textContent=draft ? JSON.stringify(draft,null,2) : ''; }
    function changed() { dirty=true; preview(); notice('有未保存的规则。保存后需重启 DriveBridge 才会生效。'); }
    function availability() {
      $('conversion-rules-reload').disabled=busy||loading;
      $('conversion-rules-add').disabled=!draft||busy||loading;
      $('conversion-rules-save').disabled=!draft||busy||loading;
      for(const control of controls) control.element.disabled=busy||loading||control.locked();
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
        const row=make('tr'), fixed=defaultIDs.has(rule.id), policy=policies[rule.id], structural=Boolean(policy);
        const source=field(row,make('input'),'原始字符串',()=>fixed); source.value=policy?.source ?? rule.source; source.maxLength=65536;
        const mode=field(row,select(structural?[[rule.target,policy.mode]]:[['text','文本'],['newline','换行'],['delete','删除']],structural?rule.target:modes.get(rule.id) ?? (rule.target==='\n'?'newline':rule.target===''?'delete':'text')),'替换目标类型',()=>structural);
        modes.set(rule.id,mode.value);
        const target=field(row,make('input'),'替换文本',()=>structural||mode.value!=='text'); target.value=structural?policy.target:mode.value==='text'?rule.target:''; target.maxLength=65536;
        const context=field(row,select(structural?[['document',policy.context]]:[['paragraph','段落'],['table','表格']],rule.context),'作用位置',()=>fixed);
        const match=field(row,select(structural?[[rule.match,policy.match]]:[['literal','普通字符串'],['exact','完整 HTML 标签'],['simple_html_break','HTML 换行标签']],rule.match),'匹配方式',()=>fixed);
        const remove=field(row,make('button','删除'),'删除此规则',()=>fixed); remove.type='button'; remove.className='button small';
        if(fixed) { source.title=context.title=match.title='内置规则的匹配边界固定；可以修改替换目标。'; remove.title='内置规则不能删除；可将目标设为“删除”以移除匹配内容。'; }
        if(structural) { source.title=mode.title=target.title=context.title=match.title=remove.title='内置结构规则固定；完整保留内容，不支持删除或改写。'; $('conversion-rules-rows').append(row); continue; }
        const newline=mode.children[1]; newline.disabled=rule.match==='literal';
        source.addEventListener('input',()=>{rule.source=source.value; changed();});
        target.addEventListener('input',()=>{if(mode.value==='text') rule.target=target.value; changed();});
        mode.addEventListener('change',()=>{
          if(mode.value==='newline'&&rule.match==='literal') { mode.value='text'; }
          modes.set(rule.id,mode.value);
          rule.target=mode.value==='newline'?'\n':mode.value==='delete'?'':target.value;
          availability(); changed();
        });
        context.addEventListener('change',()=>{rule.context=context.value;changed();});
        match.addEventListener('change',()=>{
          rule.match=match.value; newline.disabled=rule.match==='literal';
          if(rule.match==='literal'&&mode.value==='newline') {mode.value='text';modes.set(rule.id,'text');rule.target=target.value;}
          availability();changed();
        });
        remove.addEventListener('click',()=>{draft.rules=draft.rules.filter(item=>item!==rule); modes.delete(rule.id); render(); changed();});
        $('conversion-rules-rows').append(row);
      }
      preview();availability();
    }
    async function load() {
      if(loaded) return;
      if(flight) return flight;
      loading=true;availability();notice('正在读取替换规则…');
      flight=(async()=>{
        try {
          const view=await api('/api/conversion-rules');
          draft=JSON.parse(JSON.stringify(view.document)); revision=view.revision; loaded=true; dirty=false; modes.clear();
          $('conversion-rules-path').textContent=view.path;render();
          notice(view.restart_required?restartMessage:'规则保存在本机；保存后重启 DriveBridge 生效。');
        } catch(error) { loaded=Boolean(draft); notice(error.code==='unavailable'?'此管理入口暂不支持编辑替换规则。':error.message,true); }
        finally { loading=false;flight=null;availability(); }
      })();
      return flight;
    }
    async function save() {
      if(!draft||busy||loading) return;
      if(draft.rules.some(rule=>modes.get(rule.id)==='text'&&rule.target==='')) {
        notice('请填写替换文本，或将目标类型明确选择为“删除”。',true); return;
      }
      busy=true;availability();notice('正在保存规则…');
      try {
        const view=await api('/api/conversion-rules','PUT',{revision,document:JSON.parse(JSON.stringify(draft))});
        revision=view.revision; draft=JSON.parse(JSON.stringify(view.document)); $('conversion-rules-path').textContent=view.path;
        dirty=false;modes.clear();render();notice(view.restart_required?restartMessage:'已保存，当前规则已生效。');
      } catch(error) { notice(`${error.message || '保存未完成。'} 未保存的编辑仍保留在此页。`,true); }
      finally {busy=false;availability();}
    }
    $('conversion-rules-add').addEventListener('click',()=>{
      if(!draft||busy||loading) return;
      let id; do {id=`custom-${Date.now().toString(36)}-${++serial}`;} while(draft.rules.some(rule=>rule.id===id));
      draft.rules.push({id,source:'',target:'',context:'paragraph',match:'literal'}); modes.set(id,'text'); render();changed();
    });
    $('conversion-rules-save').addEventListener('click',save);
    $('conversion-rules-reload').addEventListener('click',async()=>{
      if(busy||loading) return;
      if(dirty&&!confirm('重新读取将放弃此页尚未保存的替换规则，是否继续？')) return;
      loaded=false; await load();
    });
    availability();
    return {load};
  }
  return {create};
});
