'use strict';
window.edpInitEditor = async function(container, signal) {
  const page = container.closest('#page-content');
  const find = selector => page.querySelector(selector);
  const status = find('#editor-status');
  let editor, dirty = false, previewRequest = 0;
  const listen = (element, event, handler) => element?.addEventListener(event, handler, {signal});
  const notice = message => {status.textContent = message; window.edpNotice?.(message);};
  window.edpPageDirty = () => dirty;
  const warn = event => {if(dirty){event.preventDefault();event.returnValue='';}};
  window.addEventListener('beforeunload', warn);
  signal.addEventListener('abort', () => {window.removeEventListener('beforeunload',warn);window.edpPageDirty=null;editor?.destroy();}, {once:true});
  try {
    const response = await fetch(container.dataset.url, {signal});
    if (!response.ok) throw new Error('Could not load template');
    const data = await response.json();
    if (signal.aborted || !container.isConnected) return;
    let version = data.current_version;
    editor = grapesjs.init({container:find('#grapes-editor'),height:'100%',telemetry:false,cssIcons:'/assets/editor-icons.css',storageManager:false,fromElement:false,plugins:['grapesjs-preset-newsletter'],pluginsOpts:{'grapesjs-preset-newsletter':{useCustomTheme:false}},assetManager:{upload:false},allowScripts:false,canvas:{scripts:[],styles:['/assets/email-fonts.css']}});
    if (data.project&&Object.keys(data.project).length) editor.loadProjectData(data.project);
    else editor.setComponents(data.html);
    status.textContent='Drag blocks into the message. Saved versions remain available to approved campaigns.';
    editor.on('update', () => {dirty=true;status.textContent='Unsaved changes';});
    listen(find('#template-subject'), 'input', () => {dirty=true;});
    listen(find('#template-plain'), 'input', () => {dirty=true;});
    listen(find('#insert-variable'), 'click', () => {
      const name=prompt('Variable name, for example name');
      if (!name||!/^[A-Za-z][A-Za-z0-9_]*$/.test(name)) return;
      const selected=editor.getSelected();
      if (selected&&selected.is('text')) selected.components('{{'+name+'}}');
      else editor.addComponents('<p>{{'+name+'}}</p>');
    });
    const templateInput = () => ({subject:find('#template-subject').value,html:editor.runCommand('gjs-get-inlined-html'),plain:find('#template-plain').value,project:{}});
    const exampleValue = name => {
      if (name==='school_name') return 'Example School';
      if (name==='certificate_url') return 'https://example.test/certificate';
      if (name==='logo_url') return find('#logo-url')?.value.trim() || 'https://example.test/logo.png';
      if (name==='certificate_items') return JSON.stringify([{name:'Example Recipient',award:'Participation',url:'https://example.test/certificate'}]);
      if (name==='certificate_count') return '1';
      if (name.endsWith('_url') || name==='url') return 'https://example.test';
      if (name==='email' || name.endsWith('_email')) return 'person@example.test';
      if (name==='name' || name==='recipient_name') return 'Example Recipient';
      return 'Example '+name.replaceAll('_',' ');
    };
    const previewVariables = input => {
      const field=find('#preview-variables');
      if (!field) return {};
      let values;
      try {values=JSON.parse(field.value || '{}');} catch {throw new Error('Preview variables must be a valid JSON object.');}
      if (!values || typeof values!=='object' || Array.isArray(values) || Object.values(values).some(value=>typeof value!=='string')) throw new Error('Preview variables must be an object with text values.');
      const names=new Set([...(data.variables || []),...Array.from((input.subject+input.html+input.plain).matchAll(/\{\{\s*([A-Za-z][A-Za-z0-9_]*)\s*\}\}/g),match=>match[1])]);
      let added=false;
      for (let name of names) {
        if (name==='certificate_list') name='certificate_items';
        if (!Object.hasOwn(values,name)) {Object.defineProperty(values,name,{value:exampleValue(name),enumerable:true,writable:true,configurable:true});added=true;}
      }
      if (added || field.value==='{}') field.value=JSON.stringify(values,null,2);
      return values;
    };
    const refreshPreview = async () => {
      const previewStatus=find('#preview-status');
      const button=find('#refresh-preview');
      if (!container.dataset.previewUrl || !previewStatus) return;
      const current=++previewRequest;
      button.disabled=true;
      previewStatus.textContent='Rendering preview…';
      try {
        const input=templateInput();
        const variables=previewVariables(input);
        const result=await fetch(container.dataset.previewUrl,{method:'POST',signal,headers:{'Content-Type':'application/json','X-CSRF-Token':container.dataset.csrf},body:JSON.stringify({template:input,variables})});
        const out=await result.json();
        if (!result.ok) throw new Error(out.error || 'Preview could not be rendered');
        if (signal.aborted || current!==previewRequest) return;
        find('#preview-subject').textContent=out.subject;
        const frame=find('#email-preview');
        frame.setAttribute('sandbox','');
        frame.setAttribute('referrerpolicy','no-referrer');
        frame.srcdoc=out.document || '<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="color-scheme" content="light"><style>html,body{background:#fff;color:#222}body{margin:0;padding:20px;font-family:Arial,sans-serif}img{max-width:100%;height:auto}table{max-width:100%}</style></head><body>'+out.html+'</body></html>';
        if (find('#preview-plain')) find('#preview-plain').textContent=out.plain;
        previewStatus.textContent='Preview of your current changes with sample variables. Nothing was saved or sent.';
      } catch(error) {
        if (!signal.aborted && current===previewRequest) previewStatus.textContent=error.message;
      } finally {
        if (!signal.aborted && current===previewRequest) button.disabled=false;
      }
    };
    const setPreviewWidth = () => {
      const frame=find('#email-preview');
      if (frame) frame.style.width=(find('#preview-width')?.value==='375'?'375':'640')+'px';
    };
    listen(find('#preview-width'), 'change', setPreviewWidth);
    setPreviewWidth();
    listen(find('#refresh-preview'), 'click', refreshPreview);
    try {previewVariables({subject:data.subject||'',html:data.html||'',plain:data.plain||''});} catch(error) {if(find('#preview-status'))find('#preview-status').textContent=error.message;}
    const tabs=Array.from(page.querySelectorAll('[data-editor-tab]'));
    const panels={design:find('#editor-design'),preview:find('#editor-preview'),plain:find('#editor-plain-panel')};
    const selectTab = name => {
      for (const tab of tabs) {const active=tab.dataset.editorTab===name;tab.setAttribute('aria-selected',String(active));tab.tabIndex=active?0:-1;tab.classList.toggle('active',active);tab.classList.toggle('btn-primary',active);tab.classList.toggle('btn-outline-secondary',!active);}
      for (const [key,panel] of Object.entries(panels)) if(panel) panel.hidden=key!==name;
      if(name==='preview') refreshPreview();
      if(name==='design') editor.refresh();
    };
    for (const tab of tabs) {
      listen(tab,'click',()=>selectTab(tab.dataset.editorTab));
      listen(tab,'keydown',event=>{
        let index=tabs.indexOf(tab);
        if(event.key==='ArrowRight') index=(index+1)%tabs.length;
        else if(event.key==='ArrowLeft') index=(index+tabs.length-1)%tabs.length;
        else if(event.key==='Home') index=0;
        else if(event.key==='End') index=tabs.length-1;
        else return;
        event.preventDefault();tabs[index].focus();selectTab(tabs[index].dataset.editorTab);
      });
    }
    if(tabs.length)selectTab('design');
    const logoURL = value => {
      let url;
      try {url=new URL(value);} catch {throw new Error('Use an absolute HTTP or HTTPS logo URL.');}
      if(!['https:','http:'].includes(url.protocol)||url.username||url.password)throw new Error('Use an HTTP or HTTPS logo URL without embedded credentials.');
      return url.href;
    };
    const insertLogo = async () => {
      const button=find('#insert-logo');
      const file=find('#logo-file')?.files?.[0];
      button.disabled=true;
      try {
        let source;
        if(file) {
          if(file.size>1024*1024)throw new Error('Logo files must be 1 MiB or smaller.');
          if(!['image/png','image/jpeg','image/gif'].includes(file.type))throw new Error('Choose a PNG, JPEG, or GIF logo.');
          if(!container.dataset.assetUrl)throw new Error('Image uploads are unavailable.');
          const form=new FormData();form.append('file',file);
          const result=await fetch(container.dataset.assetUrl,{method:'POST',signal,headers:{'X-CSRF-Token':container.dataset.csrf},body:form});
          const out=await result.json();
          if(!result.ok)throw new Error(out.error||'Logo upload failed');
          source=logoURL(out.url);
        } else source=logoURL(find('#logo-url')?.value.trim() || '');
        if(signal.aborted)return;
        editor.AssetManager.add({src:source,name:file?.name || 'Email logo'});
        editor.addComponents({type:'image',attributes:{src:source,alt:'Logo',width:'96'},style:{width:'96px','max-width':'100%',height:'auto',display:'block',margin:'0 auto 20px'}});
        if(find('#logo-url'))find('#logo-url').value=source;
        if(find('#logo-file'))find('#logo-file').value='';
        selectTab('design');dirty=true;status.textContent='Logo inserted. Save a new version when ready.';
      } catch(error) {if(!signal.aborted)notice(error.message);}
      finally {if(!signal.aborted)button.disabled=false;}
    };
    listen(find('#insert-logo'),'click',insertLogo);
    listen(find('#save-template'), 'click', async () => {
      const button=find('#save-template');button.disabled=true;
      try {
        const input=templateInput();input.project=editor.getProjectData();input.version=version;
        const result=await fetch(container.dataset.url,{method:'POST',signal,headers:{'Content-Type':'application/json','X-CSRF-Token':container.dataset.csrf},body:JSON.stringify(input)});
        const out=await result.json();
        if (!result.ok) throw new Error(out.error||'Save failed');
        if (signal.aborted) return;
        version=out.version;find('#editor-version').textContent=version;
        status.textContent='Version '+version+' saved. Variables: '+(out.variables.join(', ')||'none');
        dirty=false;window.edpNotice?.('Template version '+version+' saved');
      } catch (error) {if(!signal.aborted)notice(error.message);}
      finally {if(!signal.aborted)button.disabled=false;}
    });
  } catch(error) {if(!signal.aborted)status.textContent=error.message;}
};
