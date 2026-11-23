// Agent 页面只通过套件桥读取数据，所有用户内容均作为文本节点呈现。
'use strict';
(() => {
  const page = location.pathname.split('/').pop();
  const app = document.querySelector('#app');
  const dialog = document.querySelector('#dialog');
  const api = window.AlonaBot;
  const keys = ['search','refresh','empty','loading','new_persona','edit','delete','cancel','save','name','prompt','references','sessions','reconnect','tools','reload','skill_hint','problems','memory','user','conversations','view','scope','instance','entries','add_entry','confirm_delete','task','creator','target','trigger','next_at','created_at','close','disconnected','connected','error','count','images','role','kind','speaker'];
  let words = {}, state, filter = '', currentPage = 1, section = 'conversations', targetFilter = '';
  let localeVersion = 0;
  const pageSize = 8;
  const text = key => words[key] || `pages.${key}`;
  function node(tag, value, className) {
    const element = document.createElement(tag);
    if (value !== undefined) element.textContent = value;
    if (className) element.className = className;
    return element;
  }
  function label(key) { const element = node('span',text(key)); element.dataset.i18n = key; return element; }
  function skeleton(rows) {
    const box = node('div',undefined,'skeleton-list'); box.setAttribute('role','status'); box.setAttribute('aria-label',text('loading'));
    for (let index = 0; index < rows; index++) box.append(node('span',undefined,'skeleton'));
    return box;
  }
  function button(key, action, className) {
    const element = node('button',undefined,className);
    element.type = 'button'; element.append(label(key)); element.addEventListener('click',action); return element;
  }
  function errorAt(region, error) {
    const references = error.references?.map(ref => `${ref.name} (${ref.sessions})`).join(', ');
    region.textContent = [error.message, error.code, references].filter(Boolean).join(' · ');
    region.hidden = false;
  }
  async function request(method, path, body) {
    const response = await api.request(method, `/pages/api/${path}`, body);
    if (!response.ok || response.status < 200 || response.status >= 300) {
      const error = new Error(response.data?.description || response.body || `HTTP ${response.status}`);
      error.code = response.data?.code; error.references = response.data?.references; throw error;
    }
    return response.data;
  }
  function errorRegion() { const region = node('p',undefined,'error'); region.setAttribute('role','alert'); region.hidden = true; return region; }
  async function load() {
    const error = document.querySelector('#page-error'); error.hidden = true;
    app.setAttribute('aria-busy','true');
    try {
      let next;
      if (page === 'memory') {
        const [conversations, memory] = await Promise.all([request('GET','conversation'),request('GET','memory')]);
        next = {...conversations,...memory};
      } else next = await request('GET',page);
      state = next; renderList();
    } catch (failure) { errorAt(error,failure); }
    finally { app.setAttribute('aria-busy','false'); }
  }
  function openDialog(title, content, save) {
    dialog.replaceChildren();
    const heading = node('h2');
    if (typeof title === 'string') heading.textContent = title; else heading.append(title);
    heading.id = 'dialog-title'; dialog.append(heading);
    const form = node('form'), error = errorRegion(), actions = node('div',undefined,'dialog-actions');
    form.append(content,error); actions.append(button('cancel',() => dialog.close()));
    if (save) {
      const submit = button('save',() => {}); submit.type = 'submit'; submit.classList.add('ab-btn-primary'); actions.append(submit);
      form.addEventListener('submit',async event => {
        event.preventDefault(); submit.disabled = true; error.hidden = true;
        try { await save(); dialog.close(); await load(); } catch (failure) { errorAt(error,failure); }
        finally { submit.disabled = false; }
      });
    }
    form.append(actions); dialog.append(form); dialog.showModal();
  }
  function field(key, control) { const row = node('label',undefined,'field'); row.append(label(key),control); return row; }
  function editPersona(persona) {
    const fields = node('div'), name = node('input'), prompt = node('textarea');
    name.required = true; name.value = persona?.name || ''; name.readOnly = !!persona;
    prompt.value = persona?.prompt || ''; prompt.rows = 10;
    fields.append(field('name',name),field('prompt',prompt));
    openDialog(label(persona ? 'edit' : 'new_persona'),fields,() => request(persona ? 'PUT' : 'POST','personas',{name:name.value,prompt:prompt.value}));
  }
  function confirmDelete(title, action) {
    const content = node('p'); content.append(label('confirm_delete'),` ${title}`);
    openDialog(label('delete'),content,action);
  }
  function editMemory(record) {
    const content = node('div'), rows = node('div'), editors = [];
    const add = (value, original) => {
      const row = node('div',undefined,'memory-entry'), input = node('textarea',undefined,'entry-input');
      input.value = value; input.rows = 3; input.required = true;
      const editor = {input,original,removed:false}; editors.push(editor);
      row.append(input,button('delete',() => {editor.removed = true; row.remove();})); rows.append(row);
    };
    record.entries.forEach(entry => add(entry,entry));
    content.append(node('p',`${record.scope} · ${record.instance}`,'mono'),rows,button('add_entry',() => add('')));
    openDialog(label(record.target),content,() => {
      const operations = editors.flatMap(editor => {
        if (editor.original === undefined) return editor.removed ? [] : [{op:'add',text:editor.input.value}];
        if (editor.removed) return [{op:'remove',old_text:editor.original}];
        return editor.input.value === editor.original ? [] : [{op:'patch',old_text:editor.original,text:editor.input.value}];
      });
      if (!operations.length) return Promise.resolve();
      return request('PATCH','memory',{target:record.target,scope:record.scope,instance:record.instance,operations});
    });
  }
  function showTools(server) {
    const content = node('div',undefined,'tool-list');
    for (const tool of server.tools) {
      const row = node('article'); row.append(node('strong',tool.name,'mono'),node('span',['Unspecified','ReadOnly','Write','Destructive'][tool.effect],'ab-tag'),node('p',tool.description)); content.append(row);
    }
    if (!server.tools.length) content.append(label('empty'));
    openDialog(server.name,content);
  }
  async function showConversation(key) {
    const content = node('div',undefined,'conversation'), progress = skeleton(3), error = errorRegion();
    content.append(progress,error); openDialog(key,content);
    const form = dialog.querySelector('form'); form.lastChild.replaceChildren(button('close',() => dialog.close()));
    let from = 0;
    try {
      let more;
      do {
        const result = await request('GET',`conversation?${new URLSearchParams({key,from_seq:String(from)})}`);
        if (!dialog.open || !content.isConnected) return;
        for (const stored of result.entries) {
          const entry = stored.entry, article = node('article',undefined,'conversation-entry');
          article.append(node('div',[`#${stored.seq}`,entry.role,entry.kind,entry.speaker].filter(Boolean).join(' · '),'muted mono'));
          article.append(node('pre',entry.content));
          if (entry.images?.length) {
            const images = node('p',undefined,'mono'); images.append(label('images'),`: ${entry.images.map(number => `#${number}`).join(', ')}`); article.append(images);
          }
          content.append(article);
        }
        from = result.next_seq; more = result.more;
      } while (more);
    } catch (failure) { errorAt(error,failure); }
    finally { progress.remove(); }
  }
  function card(primary, secondary, actions, extra) {
    const row = node('article',undefined,'list-row'), identity = node('div',undefined,'identity');
    identity.append(node('strong',primary));
    if (secondary) identity.append(node('div',secondary,'muted secondary'));
    row.append(identity);
    if (extra) row.append(extra);
    const controls = node('div',undefined,'row-actions'); controls.append(...actions); row.append(controls); return row;
  }
  function renderList() {
    const list = document.querySelector('#list'), footer = document.querySelector('#pagination');
    // 首次加载还没有数据时用骨架占位；刷新时保留旧列表，由 #app[aria-busy] 变淡。
    if (!state) { list.replaceChildren(skeleton(4)); return; }
    list.replaceChildren(); footer.replaceChildren();
    let records = page === 'personas' ? state.personas : page === 'mcp' ? state.servers : page === 'skills' ? [...state.skills,...state.problems.map(problem => ({...problem,problem:true}))] : page === 'tasks' ? state.tasks : section === 'conversations' ? state.conversations : state.records.filter(record => record.target === section);
    records = records.filter(record => JSON.stringify(record).toLocaleLowerCase().includes(filter.toLocaleLowerCase()) && (!targetFilter || record.target?.chat === targetFilter));
    const pages = Math.max(1,Math.ceil(records.length/pageSize)); currentPage = Math.min(currentPage,pages);
    if (!records.length) list.append(node('p',text('empty'),'empty muted'));
    if (page === 'tasks' && records.length) {
      const table = node('table'), head = node('thead'), tr = node('tr');
      for (const key of ['task','target','creator','trigger','next_at','delete']) tr.append(node('th',text(key)));
      head.append(tr); table.append(head); const body = node('tbody');
      for (const task of records.slice((currentPage-1)*pageSize,currentPage*pageSize)) {
        const row = node('tr');
        const trigger = task.at || (task.interval_seconds ? `${task.interval_seconds}s` : `${task.cron || ''} ${task.timezone || ''}`);
        const columns = ['task','target','creator','trigger','next_at'];
        const values = [task.prompt,`${task.target.instance} · ${task.target.chat} ${task.target.thread}`,`${task.creator.instance} · ${task.creator.user}`,trigger,task.next_at ? new Date(task.next_at).toLocaleString() : '—'];
        values.forEach((value,index) => {const cell=node('td',value);cell.dataset.label=text(columns[index]);row.append(cell);});
        row.firstChild.append(node('div',task.key,'muted mono secondary'),node('div',`${text('created_at')}: ${new Date(task.created_at).toLocaleString()}`,'muted secondary'));
        const actions = node('td'); actions.dataset.label=text('delete'); actions.append(button('delete',() => confirmDelete(task.prompt,() => request('DELETE',`tasks?${new URLSearchParams({key:task.key})}`)),'ab-btn-danger')); row.append(actions); body.append(row);
      }
      table.append(body); list.append(table);
    } else for (const record of records.slice((currentPage-1)*pageSize,currentPage*pageSize)) {
      if (page === 'personas') {
        const references = record.references.map(ref => `${ref.name} · ${ref.sessions} ${text('sessions')}`).join(', ');
        const summary = node('p',record.prompt,'summary'); summary.title = record.prompt;
        list.append(card(record.name,references,[button('edit',() => editPersona(record)),button('delete',() => confirmDelete(record.name,() => request('DELETE',`personas?${new URLSearchParams({name:record.name})}`)),'ab-btn-danger')],summary));
      } else if (page === 'mcp') {
        const status = node('div',`${text(record.status)} · ${record.tools.length} ${text('tools')}`,`status ${record.status}`);
        if (record.error) status.append(node('p',record.error,'error'));
        list.append(card(record.name,record.transport,[button('tools',() => showTools(record)),button('reconnect',async event => {
          const control = event.currentTarget, error = document.querySelector('#page-error'); control.disabled = true;
          try { const next = await request('POST','mcp/reconnect',{name:record.name}); state.servers[state.servers.indexOf(record)] = next; renderList(); }
          catch (failure) {errorAt(error,failure);} finally {control.disabled = false;}
        })],status));
      } else if (page === 'skills') {
        list.append(card(record.problem ? record.path : record.name,record.problem ? record.message : record.description,[]));
        if (record.problem) list.lastChild.classList.add('problem');
      } else if (section === 'conversations') list.append(card(record.key,'',[button('view',() => showConversation(record.key))]));
      else {
        const preview = node('pre',record.entries.join('\n'),'memory-preview');
        list.append(card(record.scope,record.instance,[button('edit',() => editMemory(record)),button('delete',() => confirmDelete(record.scope,() => request('DELETE','memory',{target:record.target,scope:record.scope,instance:record.instance})),'ab-btn-danger')],preview));
      }
    }
    // 没有记录时列表里已有空状态，不再显示计数；只有一页时不显示页码。
    if (records.length) footer.append(node('span',`${text('count')}: ${records.length}`,'muted'));
    if (pages > 1) {
      const controls = node('nav',undefined,'page-numbers'); controls.setAttribute('aria-label',text('entries'));
      for (let number=1;number<=pages;number++) {
        const control = node('button',String(number)); control.type = 'button';
        if (number === currentPage) { control.setAttribute('aria-current','page'); control.disabled = true; }
        control.addEventListener('click',() => {currentPage=number;renderList();}); controls.append(control);
      }
      footer.append(controls);
    }
    if (page === 'tasks') {
      const select = document.querySelector('#target-filter'), selected = select.value;
      select.replaceChildren(node('option',text('target'))); select.firstChild.value = '';
      for (const target of [...new Set(state.tasks.map(task => task.target.chat))]) {const option=node('option',target);option.value=target;select.append(option);}
      select.value = selected;
    }
  }
  function mount() {
    const previousError = document.querySelector('#page-error');
    const toolbar = node('div',undefined,'toolbar'), search = node('input'), actions = node('div',undefined,'toolbar-actions');
    search.type = 'search'; search.placeholder = text('search'); search.setAttribute('aria-label',text('search')); search.value = filter;
    search.addEventListener('input',() => {filter=search.value;currentPage=1;renderList();}); toolbar.append(search);
    if (page === 'tasks') {
      const select=node('select');select.id='target-filter';select.setAttribute('aria-label',text('target'));
      select.addEventListener('change',() => {targetFilter=select.value;currentPage=1;renderList();});toolbar.append(select);
    }
    if (page === 'memory') {
      const tabs=node('div',undefined,'tabs');
      for (const key of ['conversations','memory','user']) {const tab=button(key,() => {section=key;currentPage=1;mount();});if (section===key) tab.setAttribute('aria-current','page');tabs.append(tab);}
      app.replaceChildren(tabs);
    } else app.replaceChildren();
    actions.append(button('refresh',load));
    if (page === 'personas') actions.append(button('new_persona',() => editPersona(),'ab-btn-primary'));
    if (page === 'skills') actions.append(button('reload',async event => {
      const control=event.currentTarget;control.disabled=true;
      try {state=await request('POST','skills/reload',{});renderList();}catch (failure) {errorAt(document.querySelector('#page-error'),failure);}finally{control.disabled=false;}
    }));
    toolbar.append(actions); const error=errorRegion();error.id='page-error';
    if (previousError && !previousError.hidden) {error.textContent=previousError.textContent;error.hidden=false;}
    const list=node('section',undefined,'ab-card list');list.id='list';const footer=node('footer');footer.id='pagination';
    app.append(toolbar,error,list,footer);
    if (page==='skills') app.append(node('p',text('skill_hint'),'muted'));
    renderList();
  }
  async function translate() {
    const version=++localeVersion;
    try {
      const entries=await Promise.all(keys.map(async key => [key,await api.t(`pages.${key}`)]));
      if (version!==localeVersion)return;words=Object.fromEntries(entries);mount();
      for (const element of dialog.querySelectorAll('[data-i18n]')) element.textContent=text(element.dataset.i18n);
    } catch (failure) {const error=document.querySelector('#page-error');if(error)errorAt(error,failure);}
  }
  api.onLocale(locale => {document.documentElement.lang=locale;void translate();});
  api.onTheme(theme => {document.documentElement.style.colorScheme=theme;});
  mount(); void load();
})();
