// Agent 页面只通过套件桥读取数据，所有用户内容均作为文本节点呈现。
'use strict';
(() => {
  const page = location.pathname.split('/').pop();
  const app = document.querySelector('#app');
  const dialog = document.querySelector('#dialog');
  const api = window.AlonaBot;
  const keys = [
    'search', 'refresh', 'empty', 'loading', 'new_persona', 'edit', 'delete', 'cancel', 'save',
    'name', 'prompt', 'references', 'sessions', 'reconnect', 'tools', 'reload', 'skill_hint',
    'problems', 'memory', 'user', 'conversations', 'view', 'scope', 'instance', 'entries',
    'add_entry', 'confirm_delete', 'task', 'creator', 'target', 'trigger', 'next_at',
    'created_at', 'close', 'disconnected', 'connected', 'error', 'count', 'images', 'role',
    'kind', 'speaker',
    'add_mcp', 'import_clipboard', 'manual_config', 'clipboard_import', 'transport', 'transport_stdio_desc', 'transport_http_desc', 'command',
    'args', 'args_hint', 'url', 'env', 'headers', 'add_pair', 'key_placeholder',
    'value_placeholder', 'secret_keep_placeholder', 'read_clipboard', 'clipboard_placeholder',
    'clipboard_unsupported', 'clipboard_empty', 'parse_invalid', 'detected_servers',
    'fill_manual', 'import_now', 'config_source', 'new_skill', 'upload_skill', 'description',
    'skill_body', 'skill_name_hint', 'skill_body_placeholder', 'archive_file',
    'archive_drop_hint', 'overwrite_existing', 'uploading', 'upload_btn', 'external_source',
  ];
  let words = {}, state, filter = '', currentPage = 1, section = 'conversations', targetFilter = '';
  let localeVersion = 0, loadFailure = null;
  const pageSize = 8;
  const text = key => words[key] || `pages.${key}`;

  function node(tag, value, className) {
    const element = document.createElement(tag);
    if (value !== undefined) element.textContent = value;
    if (className) element.className = className;
    return element;
  }
  function label(key) { const element = node('span', text(key)); element.dataset.i18n = key; return element; }
  function skeleton(rows) {
    const box = node('div', undefined, 'skeleton-list');
    box.setAttribute('role', 'status');
    box.setAttribute('aria-label', text('loading'));
    for (let index = 0; index < rows; index++) box.append(node('span', undefined, 'skeleton'));
    return box;
  }
  function button(key, action, className) {
    const element = node('button', undefined, className);
    element.type = 'button';
    element.append(label(key));
    element.addEventListener('click', action);
    return element;
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
      error.code = response.data?.code;
      error.references = response.data?.references;
      throw error;
    }
    return response.data;
  }
  function errorRegion() {
    const region = node('p', undefined, 'error');
    region.setAttribute('role', 'alert');
    region.hidden = true;
    return region;
  }
  async function load() {
    const error = document.querySelector('#page-error');
    if (error) error.hidden = true;
    app.setAttribute('aria-busy', 'true');
    try {
      let next;
      if (page === 'memory') {
        const [conversations, memory] = await Promise.all([request('GET', 'conversation'), request('GET', 'memory')]);
        next = { ...conversations, ...memory };
      } else next = await request('GET', page);
      state = next;
      renderList();
    } catch (failure) {
      if (error) errorAt(error, failure); else loadFailure = failure;
    } finally {
      app.setAttribute('aria-busy', 'false');
    }
  }
  function openDialog(title, content, save, submitKey = 'save') {
    dialog.replaceChildren();
    const heading = node('h2');
    if (typeof title === 'string') heading.textContent = title; else heading.append(title);
    heading.id = 'dialog-title';
    dialog.append(heading);
    const form = node('form'), error = errorRegion(), actions = node('div', undefined, 'dialog-actions');
    form.append(content, error);
    actions.append(button('cancel', () => dialog.close()));
    if (save) {
      const submit = button(submitKey, () => {});
      submit.type = 'submit';
      submit.classList.add('ab-btn-primary');
      actions.append(submit);
      form.addEventListener('submit', async event => {
        event.preventDefault();
        submit.disabled = true;
        error.hidden = true;
        try {
          await save();
          dialog.close();
          await load();
        } catch (failure) {
          errorAt(error, failure);
        } finally {
          submit.disabled = false;
        }
      });
    }
    form.append(actions);
    dialog.append(form);
    dialog.showModal();
  }
  function field(key, control, hintKey) {
    const row = node('label', undefined, 'field');
    row.append(label(key), control);
    if (hintKey) row.append(node('span', text(hintKey), 'muted secondary'));
    return row;
  }

  // ---- MCP 剪贴板与 JSON 自动识别解析器 ----
  function normalizePairs(raw) {
    if (!raw) return [];
    if (Array.isArray(raw)) {
      return raw
        .filter(item => item && typeof item.name === 'string' && item.name.trim())
        .map(item => ({ name: item.name.trim(), value: String(item.value ?? '') }));
    }
    if (typeof raw === 'object') {
      return Object.entries(raw)
        .filter(([k]) => String(k).trim() !== '')
        .map(([k, v]) => ({ name: String(k).trim(), value: String(v ?? '') }));
    }
    return [];
  }

  function deriveNameFromEntry(entry, fallbackIndex) {
    if (typeof entry.name === 'string' && entry.name.trim()) return entry.name.trim();
    const rawUrl = entry.url || entry.serverUrl || entry.httpUrl;
    if (typeof rawUrl === 'string' && rawUrl.trim()) {
      try {
        const host = new URL(rawUrl.trim()).hostname.replace(/[^a-zA-Z0-9_-]/g, '-');
        if (host) return host;
      } catch {
        // fallback below
      }
    }
    if (typeof entry.command === 'string' && entry.command.trim()) {
      const base = entry.command.trim().split(/[\\/]/).pop().replace(/[^a-zA-Z0-9_-]/g, '-');
      if (base) return base;
    }
    return `mcp-server-${fallbackIndex + 1}`;
  }

  function normalizeOneMCP(rawName, entry, index) {
    if (!entry || typeof entry !== 'object') return null;
    const rawUrl = String(entry.url || entry.serverUrl || entry.httpUrl || '').trim();
    const rawCommand = String(entry.command || '').trim();
    const rawType = String(entry.transport || entry.type || '').trim().toLowerCase();
    let transport = '';
    if (rawType === 'stdio' || (!rawUrl && rawCommand)) {
      transport = 'stdio';
    } else if (
      rawType === 'streamable-http' ||
      rawType === 'streamablehttp' ||
      rawType === 'http' ||
      rawType === 'sse' ||
      rawUrl
    ) {
      transport = 'streamable-http';
    }
    if (!transport) return null;
    const name = rawName ? String(rawName).trim() : deriveNameFromEntry(entry, index);
    if (transport === 'stdio') {
      if (!rawCommand) return null;
      const args = Array.isArray(entry.args) ? entry.args.map(item => String(item)) : [];
      return { name, transport, command: rawCommand, args, env: normalizePairs(entry.env) };
    }
    if (!rawUrl) return null;
    return { name, transport, url: rawUrl, headers: normalizePairs(entry.headers) };
  }

  function parseMCPClipboard(rawText) {
    const trimmed = rawText.trim();
    if (!trimmed) return { servers: [], error: '' };
    let data;
    try {
      data = JSON.parse(trimmed);
    } catch {
      return { servers: [], error: text('parse_invalid') };
    }
    const list = [];
    const mapContainer = data && typeof data === 'object' ? (data.mcpServers || data.servers) : null;
    if (mapContainer && typeof mapContainer === 'object' && !Array.isArray(mapContainer)) {
      let idx = 0;
      for (const [key, value] of Object.entries(mapContainer)) {
        const parsed = normalizeOneMCP(key, value, idx++);
        if (parsed) list.push(parsed);
      }
    } else if (Array.isArray(data)) {
      data.forEach((item, idx) => {
        const parsed = normalizeOneMCP('', item, idx);
        if (parsed) list.push(parsed);
      });
    } else if (data && typeof data === 'object') {
      const single = normalizeOneMCP('', data, 0);
      if (single) {
        list.push(single);
      } else {
        // 支持直接粘贴 {"serverName": {"command": ...}} 片段。
        let idx = 0;
        for (const [key, value] of Object.entries(data)) {
          const parsed = normalizeOneMCP(key, value, idx++);
          if (parsed) list.push(parsed);
        }
      }
    }
    if (!list.length) return { servers: [], error: text('parse_invalid') };
    return { servers: list, error: '' };
  }

  // ---- MCP 添加 / 编辑弹窗（手动配置 + 剪贴板导入双模式） ----
  function openMCPDialog(existingServer, initialMode = 'manual') {
    const isEdit = Boolean(existingServer);
    let activeMode = isEdit ? 'manual' : initialMode;
    let parsedClipboard = [];

    const container = node('div', undefined, 'mcp-dialog-body');
    const modeTabs = node('div', undefined, 'tabs');
    const manualPane = node('div');
    const importPane = node('div');

    // 手动配置表单控件
    const nameInput = node('input');
    nameInput.type = 'text';
    nameInput.value = existingServer?.name || '';
    nameInput.readOnly = isEdit;

    let selectedTransport = existingServer?.transport || 'stdio';
    const transportGroup = node('div', undefined, 'transport-grid');
    transportGroup.setAttribute('role', 'radiogroup');
    transportGroup.setAttribute('aria-label', text('transport'));
    const transportButtons = new Map();
    const transportOptions = [
      { value: 'stdio', title: 'stdio', descKey: 'transport_stdio_desc' },
      { value: 'streamable-http', title: 'streamable-http', descKey: 'transport_http_desc' },
    ];

    const stdioSection = node('div');
    const commandInput = node('input');
    commandInput.type = 'text';
    commandInput.placeholder = 'npx';
    commandInput.value = existingServer?.command || '';

    const argsInput = node('textarea');
    argsInput.rows = 3;
    argsInput.className = 'mono';
    argsInput.value = (existingServer?.args || []).join('\n');

    const envList = node('div', undefined, 'pair-list');
    const envEditors = [];
    const addPairRow = (listEl, editors, keyName = '', value = '', keep = false) => {
      const row = node('div', undefined, 'pair-row');
      const k = node('input');
      k.type = 'text';
      k.placeholder = text('key_placeholder');
      k.value = keyName;
      k.className = 'mono';
      const v = node('input');
      v.type = 'password';
      v.placeholder = keep ? text('secret_keep_placeholder') : text('value_placeholder');
      v.value = value;
      v.className = 'mono';
      const item = { k, v, keep, removed: false };
      editors.push(item);
      const del = button('delete', () => { item.removed = true; row.remove(); });
      row.append(k, v, del);
      listEl.append(row);
    };
    (existingServer?.env_names || []).forEach(keyName => addPairRow(envList, envEditors, keyName, '', true));
    const envBlock = node('div', undefined, 'field');
    envBlock.append(label('env'), envList, button('add_pair', () => addPairRow(envList, envEditors)));
    stdioSection.append(field('command', commandInput), field('args', argsInput, 'args_hint'), envBlock);

    const httpSection = node('div');
    const urlInput = node('input');
    urlInput.type = 'url';
    urlInput.placeholder = 'https://mcp.example.com/mcp';
    urlInput.value = existingServer?.url || '';

    const headerList = node('div', undefined, 'pair-list');
    const headerEditors = [];
    (existingServer?.header_names || []).forEach(keyName => addPairRow(headerList, headerEditors, keyName, '', true));
    const headerBlock = node('div', undefined, 'field');
    headerBlock.append(label('headers'), headerList, button('add_pair', () => addPairRow(headerList, headerEditors)));
    httpSection.append(field('url', urlInput), headerBlock);

    const setTransport = next => {
      selectedTransport = next;
      const isStdio = next === 'stdio';
      stdioSection.hidden = !isStdio;
      httpSection.hidden = isStdio;
      for (const [val, btn] of transportButtons.entries()) {
        const active = val === next;
        btn.setAttribute('aria-checked', String(active));
        btn.classList.toggle('active', active);
      }
    };
    for (const opt of transportOptions) {
      const btn = node('button', undefined, 'transport-option');
      btn.type = 'button';
      btn.setAttribute('role', 'radio');
      const head = node('div', undefined, 'transport-option-head');
      head.append(node('span', undefined, 'transport-dot'), node('strong', opt.title, 'mono'));
      btn.append(head, node('span', text(opt.descKey), 'muted secondary'));
      btn.addEventListener('click', () => setTransport(opt.value));
      transportButtons.set(opt.value, btn);
      transportGroup.append(btn);
    }
    setTransport(selectedTransport);

    const transportField = node('div', undefined, 'field');
    transportField.append(label('transport'), transportGroup);
    manualPane.append(field('name', nameInput), transportField, stdioSection, httpSection);

    // 粘贴板 / JSON 识别导入面板
    const pasteBar = node('div', undefined, 'paste-bar');
    const pasteArea = node('textarea', undefined, 'mono');
    pasteArea.rows = 8;
    pasteArea.placeholder = text('clipboard_placeholder');
    const parseHint = node('p', undefined, 'error');
    parseHint.hidden = true;
    const previewList = node('div', undefined, 'import-preview');

    const fillManualFrom = server => {
      nameInput.value = server.name || '';
      setTransport(server.transport || 'stdio');
      commandInput.value = server.command || '';
      argsInput.value = (server.args || []).join('\n');
      urlInput.value = server.url || '';
      envList.replaceChildren();
      envEditors.length = 0;
      (server.env || []).forEach(pair => addPairRow(envList, envEditors, pair.name, pair.value, false));
      headerList.replaceChildren();
      headerEditors.length = 0;
      (server.headers || []).forEach(pair => addPairRow(headerList, headerEditors, pair.name, pair.value, false));
      switchMode('manual');
    };

    const runParse = () => {
      const res = parseMCPClipboard(pasteArea.value);
      parsedClipboard = res.servers;
      previewList.replaceChildren();
      if (res.error) {
        parseHint.textContent = res.error;
        parseHint.hidden = false;
        return;
      }
      parseHint.hidden = true;
      if (!parsedClipboard.length) return;
      previewList.append(node('p', `${text('detected_servers')}: ${parsedClipboard.length}`, 'muted secondary'));
      for (const item of parsedClipboard) {
        const cardEl = node('div', undefined, 'preview-item');
        const info = node('div');
        info.append(
          node('strong', item.name),
          node('span', item.transport, 'ab-tag'),
          node('div', item.transport === 'stdio' ? [item.command, ...(item.args || [])].join(' ') : item.url, 'muted mono secondary'),
        );
        cardEl.append(info);
        if (parsedClipboard.length === 1) {
          cardEl.append(button('fill_manual', () => fillManualFrom(item)));
        }
        previewList.append(cardEl);
      }
    };
    pasteArea.addEventListener('input', runParse);

    const readClip = async () => {
      parseHint.hidden = true;
      if (!navigator.clipboard || !navigator.clipboard.readText) {
        parseHint.textContent = text('clipboard_unsupported');
        parseHint.hidden = false;
        return;
      }
      try {
        const clip = await navigator.clipboard.readText();
        if (!clip.trim()) {
          parseHint.textContent = text('clipboard_empty');
          parseHint.hidden = false;
          return;
        }
        pasteArea.value = clip;
        runParse();
      } catch {
        parseHint.textContent = text('clipboard_unsupported');
        parseHint.hidden = false;
      }
    };
    pasteBar.append(button('read_clipboard', () => { void readClip(); }));
    importPane.append(pasteBar, field('clipboard_import', pasteArea), parseHint, previewList);

    const manualTab = button('manual_config', () => switchMode('manual'));
    const importTab = button('clipboard_import', () => switchMode('import'));
    const switchMode = next => {
      activeMode = next;
      manualPane.hidden = next !== 'manual';
      importPane.hidden = next !== 'import';
      if (next === 'manual') {
        manualTab.setAttribute('aria-current', 'page');
        importTab.removeAttribute('aria-current');
      } else {
        importTab.setAttribute('aria-current', 'page');
        manualTab.removeAttribute('aria-current');
      }
    };
    if (!isEdit) {
      modeTabs.append(manualTab, importTab);
      container.append(modeTabs);
    }
    container.append(manualPane, importPane);
    switchMode(activeMode);

    const collectPairs = editors => {
      const pairs = [];
      const keep = [];
      for (const item of editors) {
        if (item.removed) continue;
        const k = item.k.value.trim();
        if (!k) continue;
        if (item.keep && item.v.value === '') {
          keep.push(k);
        } else {
          pairs.push({ name: k, value: item.v.value });
        }
      }
      return { pairs, keep };
    };

    openDialog(label(isEdit ? 'edit' : 'add_mcp'), container, async () => {
      if (activeMode === 'import') {
        runParse();
        if (!parsedClipboard.length) {
          throw new Error(text('parse_invalid'));
        }
        return request('POST', 'mcp', { servers: parsedClipboard });
      }
      const transport = selectedTransport;
      const payload = { name: nameInput.value.trim(), transport };
      if (transport === 'stdio') {
        payload.command = commandInput.value.trim();
        payload.args = argsInput.value.split('\n').map(line => line.trim()).filter(Boolean);
        const env = collectPairs(envEditors);
        payload.env = env.pairs;
        if (env.keep.length) payload.keep_env = env.keep;
      } else {
        payload.url = urlInput.value.trim();
        const hdr = collectPairs(headerEditors);
        payload.headers = hdr.pairs;
        if (hdr.keep.length) payload.keep_headers = hdr.keep;
      }
      return request(isEdit ? 'PUT' : 'POST', 'mcp', payload);
    });

    if (!isEdit && initialMode === 'import') {
      void readClip();
    }
  }

  // ---- 技能手动编辑与压缩包上传弹窗 ----
  function openSkillEditor(existingSkill, readOnly = false) {
    const isEdit = Boolean(existingSkill);
    const content = node('div');
    const nameInput = node('input', undefined, 'mono');
    nameInput.type = 'text';
    nameInput.required = true;
    nameInput.value = existingSkill?.name || '';
    nameInput.readOnly = isEdit || readOnly;
    nameInput.placeholder = 'my-custom-skill';

    const descInput = node('input');
    descInput.type = 'text';
    descInput.required = true;
    descInput.value = existingSkill?.description || '';
    descInput.readOnly = readOnly;

    const bodyInput = node('textarea', undefined, 'mono');
    bodyInput.rows = 12;
    bodyInput.value = existingSkill?.body || '';
    bodyInput.readOnly = readOnly;
    bodyInput.placeholder = text('skill_body_placeholder');

    content.append(
      field('name', nameInput, isEdit || readOnly ? undefined : 'skill_name_hint'),
      field('description', descInput),
      field('skill_body', bodyInput),
    );
    if (readOnly) {
      openDialog(existingSkill.name, content);
      return;
    }
    openDialog(label(isEdit ? 'edit' : 'new_skill'), content, () =>
      request(isEdit ? 'PUT' : 'POST', 'skills', {
        name: nameInput.value.trim(),
        description: descInput.value.trim(),
        body: bodyInput.value,
      }),
    );
  }

  function fileToBase64(file) {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => {
        const result = String(reader.result || '');
        const comma = result.indexOf(',');
        resolve(comma >= 0 ? result.slice(comma + 1) : result);
      };
      reader.onerror = () => reject(new Error('Failed to read archive file'));
      reader.readAsDataURL(file);
    });
  }

  function openSkillUpload() {
    const content = node('div');
    let selectedFile = null;

    const drop = node('label', undefined, 'upload-drop');
    const fileInput = node('input');
    fileInput.type = 'file';
    fileInput.accept = '.zip,.tar.gz,.tgz';
    const fileLabel = node('span', text('archive_drop_hint'), 'muted');
    const applyFile = file => {
      if (!file) return;
      selectedFile = file;
      const kb = Math.max(1, Math.round(file.size / 1024));
      fileLabel.textContent = `${file.name} (${kb} KB)`;
    };
    fileInput.addEventListener('change', () => applyFile(fileInput.files?.[0]));
    drop.addEventListener('dragover', event => { event.preventDefault(); drop.classList.add('dragover'); });
    drop.addEventListener('dragleave', () => drop.classList.remove('dragover'));
    drop.addEventListener('drop', event => {
      event.preventDefault();
      drop.classList.remove('dragover');
      applyFile(event.dataTransfer?.files?.[0]);
    });
    drop.append(fileInput, fileLabel);

    const overwriteRow = node('label', undefined, 'checkbox-row');
    const overwriteBox = node('input');
    overwriteBox.type = 'checkbox';
    overwriteRow.append(overwriteBox, label('overwrite_existing'));

    content.append(field('archive_file', drop), overwriteRow);
    openDialog(label('upload_skill'), content, async () => {
      if (!selectedFile) throw new Error(text('archive_drop_hint'));
      const dataBase64 = await fileToBase64(selectedFile);
      return request('POST', 'skills/upload', {
        filename: selectedFile.name,
        data_base64: dataBase64,
        overwrite: overwriteBox.checked,
      });
    }, 'upload_btn');
  }

  function editPersona(persona) {
    const fields = node('div'), name = node('input'), prompt = node('textarea');
    name.type = 'text'; name.required = true; name.value = persona?.name || ''; name.readOnly = !!persona;
    prompt.value = persona?.prompt || ''; prompt.rows = 10;
    fields.append(field('name', name), field('prompt', prompt));
    openDialog(label(persona ? 'edit' : 'new_persona'), fields, () => request(persona ? 'PUT' : 'POST', 'personas', { name: name.value, prompt: prompt.value }));
  }
  function confirmDelete(title, action) {
    const content = node('p');
    content.append(label('confirm_delete'), ` ${title}`);
    openDialog(label('delete'), content, action);
  }
  function editMemory(record) {
    const content = node('div'), rows = node('div'), editors = [];
    const add = (value, original) => {
      const row = node('div', undefined, 'memory-entry'), input = node('textarea', undefined, 'entry-input');
      input.value = value; input.rows = 3; input.required = true;
      const editor = { input, original, removed: false }; editors.push(editor);
      row.append(input, button('delete', () => { editor.removed = true; row.remove(); }));
      rows.append(row);
    };
    record.entries.forEach(entry => add(entry, entry));
    content.append(node('p', `${record.scope} · ${record.instance}`, 'mono'), rows, button('add_entry', () => add('')));
    openDialog(label(record.target), content, () => {
      const operations = editors.flatMap(editor => {
        if (editor.original === undefined) return editor.removed ? [] : [{ op: 'add', text: editor.input.value }];
        if (editor.removed) return [{ op: 'remove', old_text: editor.original }];
        return editor.input.value === editor.original ? [] : [{ op: 'patch', old_text: editor.original, text: editor.input.value }];
      });
      if (!operations.length) return Promise.resolve();
      return request('PATCH', 'memory', { target: record.target, scope: record.scope, instance: record.instance, operations });
    });
  }
  function showTools(server) {
    const content = node('div', undefined, 'tool-list');
    for (const tool of server.tools) {
      const row = node('article');
      row.append(node('strong', tool.name, 'mono'), node('span', ['Unspecified', 'ReadOnly', 'Write', 'Destructive'][tool.effect], 'ab-tag'), node('p', tool.description));
      content.append(row);
    }
    if (!server.tools.length) content.append(label('empty'));
    openDialog(server.name, content);
  }
  async function showConversation(key) {
    const content = node('div', undefined, 'conversation'), progress = skeleton(3), error = errorRegion();
    content.append(progress, error);
    openDialog(key, content);
    const form = dialog.querySelector('form');
    form.lastChild.replaceChildren(button('close', () => dialog.close()));
    let from = 0;
    try {
      let more;
      do {
        const result = await request('GET', `conversation?${new URLSearchParams({ key, from_seq: String(from) })}`);
        if (!dialog.open || !content.isConnected) return;
        for (const stored of result.entries) {
          const entry = stored.entry, article = node('article', undefined, 'conversation-entry');
          article.append(node('div', [`#${stored.seq}`, entry.role, entry.kind, entry.speaker].filter(Boolean).join(' · '), 'muted mono'));
          article.append(node('pre', entry.content));
          if (entry.images?.length) {
            const images = node('p', undefined, 'mono');
            images.append(label('images'), `: ${entry.images.map(number => `#${number}`).join(', ')}`);
            article.append(images);
          }
          content.append(article);
        }
        from = result.next_seq; more = result.more;
      } while (more);
    } catch (failure) { errorAt(error, failure); }
    finally { progress.remove(); }
  }
  function card(primary, secondary, actions, extra, badgeKey) {
    const row = node('article', undefined, 'list-row'), identity = node('div', undefined, 'identity');
    const titleLine = node('div', undefined, 'identity-head');
    titleLine.append(node('strong', primary));
    if (badgeKey) titleLine.append(node('span', text(badgeKey), 'ab-tag'));
    identity.append(titleLine);
    if (secondary) identity.append(node('div', secondary, 'muted secondary'));
    row.append(identity);
    if (extra) row.append(extra);
    const controls = node('div', undefined, 'row-actions');
    controls.append(...actions);
    row.append(controls);
    return row;
  }
  function renderList() {
    const list = document.querySelector('#list'), footer = document.querySelector('#pagination');
    if (!list) return;
    if (!state) { list.replaceChildren(skeleton(4)); return; }
    list.replaceChildren(); footer.replaceChildren();
    let records = page === 'personas' ? state.personas : page === 'mcp' ? state.servers : page === 'skills' ? [...state.skills, ...state.problems.map(problem => ({ ...problem, problem: true }))] : page === 'tasks' ? state.tasks : section === 'conversations' ? state.conversations : state.records.filter(record => record.target === section);
    records = records.filter(record => JSON.stringify(record).toLocaleLowerCase().includes(filter.toLocaleLowerCase()) && (!targetFilter || record.target?.chat === targetFilter));
    const pages = Math.max(1, Math.ceil(records.length / pageSize));
    currentPage = Math.min(currentPage, pages);
    if (!records.length) list.append(node('p', text('empty'), 'empty muted'));
    if (page === 'tasks' && records.length) {
      const table = node('table'), head = node('thead'), tr = node('tr');
      for (const key of ['task', 'target', 'creator', 'trigger', 'next_at', 'delete']) tr.append(node('th', text(key)));
      head.append(tr); table.append(head); const body = node('tbody');
      for (const task of records.slice((currentPage - 1) * pageSize, currentPage * pageSize)) {
        const row = node('tr');
        const trigger = task.at || (task.interval_seconds ? `${task.interval_seconds}s` : `${task.cron || ''} ${task.timezone || ''}`);
        const columns = ['task', 'target', 'creator', 'trigger', 'next_at'];
        const values = [task.prompt, `${task.target.instance} · ${task.target.chat} ${task.target.thread}`, `${task.creator.instance} · ${task.creator.user}`, trigger, task.next_at ? new Date(task.next_at).toLocaleString() : '—'];
        values.forEach((value, index) => { const cell = node('td', value); cell.dataset.label = text(columns[index]); row.append(cell); });
        row.firstChild.append(node('div', task.key, 'muted mono secondary'), node('div', `${text('created_at')}: ${new Date(task.created_at).toLocaleString()}`, 'muted secondary'));
        const actions = node('td'); actions.dataset.label = text('delete');
        actions.append(button('delete', () => confirmDelete(task.prompt, () => request('DELETE', `tasks?${new URLSearchParams({ key: task.key })}`)), 'ab-btn-danger'));
        row.append(actions); body.append(row);
      }
      table.append(body); list.append(table);
    } else for (const record of records.slice((currentPage - 1) * pageSize, currentPage * pageSize)) {
      if (page === 'personas') {
        const references = record.references.map(ref => `${ref.name} · ${ref.sessions} ${text('sessions')}`).join(', ');
        const summary = node('p', record.prompt, 'summary'); summary.title = record.prompt;
        list.append(card(record.name, references, [button('edit', () => editPersona(record)), button('delete', () => confirmDelete(record.name, () => request('DELETE', `personas?${new URLSearchParams({ name: record.name })}`)), 'ab-btn-danger')], summary));
      } else if (page === 'mcp') {
        const status = node('div', `${text(record.status)} · ${record.tools.length} ${text('tools')}`, `status ${record.status}`);
        if (record.error) status.append(node('p', record.error, 'error'));
        const rowActions = [
          button('tools', () => showTools(record)),
          button('reconnect', async event => {
            const control = event.currentTarget, error = document.querySelector('#page-error'); control.disabled = true;
            try { const next = await request('POST', 'mcp/reconnect', { name: record.name }); state.servers[state.servers.indexOf(record)] = next; renderList(); }
            catch (failure) { errorAt(error, failure); } finally { control.disabled = false; }
          }),
        ];
        if (record.managed) {
          rowActions.push(
            button('edit', () => openMCPDialog(record, 'manual')),
            button('delete', () => confirmDelete(record.name, () => request('DELETE', `mcp?${new URLSearchParams({ name: record.name })}`)), 'ab-btn-danger'),
          );
        }
        const detail = record.transport === 'stdio'
          ? `${record.transport}${record.command ? ` · ${record.command}` : ''}`
          : `${record.transport}${record.url ? ` · ${record.url}` : ''}`;
        list.append(card(record.name, detail, rowActions, status, record.managed ? undefined : 'config_source'));
      } else if (page === 'skills') {
        if (record.problem) {
          list.append(card(record.path, record.message, []));
          list.lastChild.classList.add('problem');
        } else {
          const skillActions = record.managed
            ? [
                button('edit', () => openSkillEditor(record, false)),
                button('delete', () => confirmDelete(record.name, () => request('DELETE', `skills?${new URLSearchParams({ name: record.name })}`)), 'ab-btn-danger'),
              ]
            : [button('view', () => openSkillEditor(record, true))];
          list.append(card(record.name, record.description, skillActions, undefined, record.managed ? undefined : 'external_source'));
        }
      } else if (section === 'conversations') list.append(card(record.key, '', [button('view', () => showConversation(record.key))]));
      else {
        const preview = node('pre', record.entries.join('\n'), 'memory-preview');
        list.append(card(record.scope, record.instance, [button('edit', () => editMemory(record)), button('delete', () => confirmDelete(record.scope, () => request('DELETE', 'memory', { target: record.target, scope: record.scope, instance: record.instance })), 'ab-btn-danger')], preview));
      }
    }
    if (records.length) footer.append(node('span', `${text('count')}: ${records.length}`, 'muted'));
    if (pages > 1) {
      const controls = node('nav', undefined, 'page-numbers'); controls.setAttribute('aria-label', text('entries'));
      for (let number = 1; number <= pages; number++) {
        const control = node('button', String(number)); control.type = 'button';
        if (number === currentPage) { control.setAttribute('aria-current', 'page'); control.disabled = true; }
        control.addEventListener('click', () => { currentPage = number; renderList(); });
        controls.append(control);
      }
      footer.append(controls);
    }
    if (page === 'tasks') {
      const select = document.querySelector('#target-filter'), selected = select.value;
      select.replaceChildren(node('option', text('target'))); select.firstChild.value = '';
      for (const target of [...new Set(state.tasks.map(task => task.target.chat))]) { const option = node('option', target); option.value = target; select.append(option); }
      select.value = selected;
    }
  }
  function mount() {
    const previousError = document.querySelector('#page-error');
    const toolbar = node('div', undefined, 'toolbar'), search = node('input'), actions = node('div', undefined, 'toolbar-actions');
    search.type = 'search'; search.placeholder = text('search'); search.setAttribute('aria-label', text('search')); search.value = filter;
    search.addEventListener('input', () => { filter = search.value; currentPage = 1; renderList(); });
    toolbar.append(search);
    if (page === 'tasks') {
      const select = node('select'); select.id = 'target-filter'; select.setAttribute('aria-label', text('target'));
      select.addEventListener('change', () => { targetFilter = select.value; currentPage = 1; renderList(); });
      toolbar.append(select);
    }
    if (page === 'memory') {
      const tabs = node('div', undefined, 'tabs');
      for (const key of ['conversations', 'memory', 'user']) {
        const tab = button(key, () => { section = key; currentPage = 1; mount(); });
        if (section === key) tab.setAttribute('aria-current', 'page');
        tabs.append(tab);
      }
      app.replaceChildren(tabs);
    } else app.replaceChildren();
    actions.append(button('refresh', load));
    if (page === 'personas') actions.append(button('new_persona', () => editPersona(), 'ab-btn-primary'));
    if (page === 'mcp') {
      actions.append(
        button('import_clipboard', () => openMCPDialog(null, 'import')),
        button('add_mcp', () => openMCPDialog(null, 'manual'), 'ab-btn-primary'),
      );
    }
    if (page === 'skills') {
      actions.append(
        button('reload', async event => {
          const control = event.currentTarget; control.disabled = true;
          try { state = await request('POST', 'skills/reload', {}); renderList(); } catch (failure) { errorAt(document.querySelector('#page-error'), failure); } finally { control.disabled = false; }
        }),
        button('upload_skill', () => openSkillUpload()),
        button('new_skill', () => openSkillEditor(null, false), 'ab-btn-primary'),
      );
    }
    toolbar.append(actions);
    const error = errorRegion(); error.id = 'page-error';
    if (previousError && !previousError.hidden) { error.textContent = previousError.textContent; error.hidden = false; }
    else if (loadFailure) { errorAt(error, loadFailure); loadFailure = null; }
    const list = node('section', undefined, 'ab-card list'); list.id = 'list';
    const footer = node('footer'); footer.id = 'pagination';
    app.append(toolbar, error, list, footer);
    if (page === 'skills') app.append(node('p', text('skill_hint'), 'muted'));
    renderList();
  }
  async function translate() {
    const version = ++localeVersion;
    try {
      const entries = await Promise.all(keys.map(async key => [key, await api.t(`pages.${key}`)]));
      if (version !== localeVersion) return;
      words = Object.fromEntries(entries);
      mount();
      for (const element of dialog.querySelectorAll('[data-i18n]')) element.textContent = text(element.dataset.i18n);
    } catch (failure) { if (version !== localeVersion) return; mount(); errorAt(document.querySelector('#page-error'), failure); }
  }
  api.onLocale(locale => { document.documentElement.lang = locale; void translate(); });
  api.onTheme(theme => { document.documentElement.style.colorScheme = theme; });
  app.replaceChildren(skeleton(4));
  void load();
})();
