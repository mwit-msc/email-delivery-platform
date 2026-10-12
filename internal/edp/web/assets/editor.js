'use strict';
window.edpInitEditor = async function(container, signal) {
  const page = container.closest('#page-content');
  const status = page.querySelector('#editor-status');
  let editor, dirty = false;
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
    editor = grapesjs.init({container:page.querySelector('#grapes-editor'),height:'100%',storageManager:false,fromElement:false,plugins:['grapesjs-preset-newsletter'],pluginsOpts:{'grapesjs-preset-newsletter':{useCustomTheme:false}},assetManager:{upload:false},allowScripts:false,canvas:{scripts:[]}});
    if (data.project&&Object.keys(data.project).length) editor.loadProjectData(data.project);
    else editor.setComponents(data.html);
    status.textContent='Drag blocks into the message. Saved versions remain available to approved campaigns.';
    editor.on('update', () => {dirty=true;status.textContent='Unsaved changes';});
    page.querySelector('#template-subject').addEventListener('input', () => {dirty=true;});
    page.querySelector('#template-plain').addEventListener('input', () => {dirty=true;});
    page.querySelector('#insert-variable').addEventListener('click', () => {
      const name=prompt('Variable name, for example name');
      if (!name||!/^[A-Za-z][A-Za-z0-9_]*$/.test(name)) return;
      const selected=editor.getSelected();
      if (selected&&selected.is('text')) selected.components('{{'+name+'}}');
      else editor.addComponents('<p>{{'+name+'}}</p>');
    });
    page.querySelector('#save-template').addEventListener('click', async () => {
      const button=page.querySelector('#save-template');button.disabled=true;
      try {
        const html=editor.runCommand('gjs-get-inlined-html');
        const result=await fetch(container.dataset.url,{method:'POST',signal,headers:{'Content-Type':'application/json','X-CSRF-Token':container.dataset.csrf},body:JSON.stringify({Subject:page.querySelector('#template-subject').value,HTML:html,Plain:page.querySelector('#template-plain').value,Project:editor.getProjectData(),Version:version})});
        const out=await result.json();
        if (!result.ok) throw new Error(out.error||'Save failed');
        if (signal.aborted) return;
        version=out.version;page.querySelector('#editor-version').textContent=version;
        status.textContent='Version '+version+' saved. Variables: '+(out.variables.join(', ')||'none');
        dirty=false;edpNotice('Template version '+version+' saved');
      } catch (error) {if(!signal.aborted){status.textContent=error.message;edpNotice(error.message);}}
      finally {if(!signal.aborted)button.disabled=false;}
    });
  } catch(error) {if(!signal.aborted)status.textContent=error.message;}
};
