'use strict';
window.edpNotice = function(message) {const n=document.getElementById('notice');n.textContent=message;n.hidden=false;setTimeout(()=>{n.hidden=true},7000);};
document.addEventListener('click',event=>{const b=event.target.closest('[data-confirm]');if(b&&!confirm(b.dataset.confirm))event.preventDefault();});
// HTML fragments and normal forms share the server's CSRF checks.
document.addEventListener('htmx:configRequest',event=>{const input=document.querySelector('input[name="csrf"]');if(input)event.detail.headers['X-CSRF-Token']=input.value;});
// Delegation keeps workspace controls working after navigation updates.
document.addEventListener('click', event => {
  const picker = document.getElementById('workspace-picker');
  if (picker && !picker.contains(event.target)) picker.open = false;
});
document.addEventListener('keydown', event => {
  const picker = document.getElementById('workspace-picker');
  if (event.key === 'Escape' && picker?.open) {
    picker.open = false;
    picker.querySelector('summary').focus();
  }
});
// Connection checks submit the current form without saving or navigating away.
document.addEventListener('submit', async event => {
  const form = event.target;
  const submitter = event.submitter;
  if (!(form instanceof HTMLFormElement) || !form.matches('.connection-form') || !submitter || !submitter.value.endsWith('.test')) return;
  event.preventDefault();
  if (form.dataset.testing === 'true') return;
  const result = form.querySelector('[data-test-result]');
  const fields = new FormData(form);
  fields.set(submitter.name, submitter.value);
  const controls = Array.from(form.querySelectorAll('button[type="submit"],button:not([type])'));
  const disabled = controls.map(button => button.disabled);
  form.dataset.testing = 'true';
  form.setAttribute('aria-busy', 'true');
  controls.forEach(button => { button.disabled = true; });
  result.hidden = false;
  result.className = 'small text-secondary mt-3 mb-0';
  result.textContent = 'Testing connection…';
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 20000);
  try {
    const response = await fetch(form.getAttribute('action') || location.pathname, {
      method: 'POST',
      body: new URLSearchParams(fields),
      credentials: 'same-origin',
      headers: { 'Accept': 'application/json', 'X-CSRF-Token': fields.get('csrf') || '' },
      signal: controller.signal,
    });
    let data;
    try { data = await response.json(); }
    catch { throw new Error(response.redirected ? 'Your session may have expired. Sign in again, then retry the connection test.' : 'EDP returned an unexpected response (' + response.status + '). Reload the page and retry.'); }
    if (!response.ok || !data.success) throw new Error(data.error || 'Connection test failed. Check your settings and try again.');
    result.className = 'small text-success mt-3 mb-0';
    result.textContent = data.message || 'Connection verified.';
  } catch (error) {
    result.className = 'small text-danger mt-3 mb-0';
    result.textContent = error.name === 'AbortError'
      ? 'Connection test timed out. Your changes have not been saved.'
      : error.message || 'Unable to reach EDP. Your changes have not been saved.';
  } finally {
    clearTimeout(timeout);
    controls.forEach((button, index) => { button.disabled = disabled[index]; });
    form.removeAttribute('aria-busy');
    delete form.dataset.testing;
  }
});
document.addEventListener('input', event => {
  if (event.target.id !== 'oidc-public') return;
  const callback = document.getElementById('oidc-callback');
  if (callback) callback.value = event.target.value.trim().replace(/\/+$/, '') + '/auth/callback';
});

// Page assets are loaded once; page-owned timers and editors stop when replaced.
const pageScripts = new Map();
function pageScript(path) {
  if (!pageScripts.has(path)) {
    pageScripts.set(path, new Promise((resolve, reject) => {
      const script = document.createElement('script');
      script.src = path;
      script.onload = resolve;
      script.onerror = () => { pageScripts.delete(path); script.remove(); reject(new Error('Could not load page tools. Reload to retry.')); };
      document.head.append(script);
    }));
  }
  return pageScripts.get(path);
}
let pageLifetime;
async function activatePage() {
  const page = document.getElementById('page-content');
  if (!page || page.dataset.initialized === 'true') return;
  page.dataset.initialized = 'true';
  pageLifetime?.abort();
  const lifetime = new AbortController();
  pageLifetime = lifetime;
  try {
    const statistics = page.querySelector('#stats-panel');
    const editor = page.querySelector('#template-editor');
    const sheet = page.querySelector('#campaign-sheet');
    if (sheet) {
      await pageScript('/assets/campaign-sheet.js');
      if (!lifetime.signal.aborted) window.edpInitSheet(sheet, lifetime.signal);
    }
    if (statistics) {
      await pageScript('/assets/chart.umd.js');
      await pageScript('/assets/charts.js');
      if (!lifetime.signal.aborted) window.edpInitCharts(statistics, lifetime.signal);
    }
    if (editor) {
      if (!document.getElementById('editor-styles')) {
        const css = document.createElement('link');
        css.id = 'editor-styles'; css.rel = 'stylesheet'; css.href = '/assets/grapes.min.css';
        document.head.append(css);
      }
      await pageScript('/assets/grapes.min.js');
      await pageScript('/assets/grapesjs-preset-newsletter.js');
      await pageScript('/assets/editor.js');
      if (!lifetime.signal.aborted) window.edpInitEditor(editor, lifetime.signal);
    }
  } catch (error) { if (!lifetime.signal.aborted) window.edpNotice(error.message); }
}
document.addEventListener('htmx:beforeRequest', event => {
  if (!event.detail.boosted) return;
  if (window.edpPageDirty?.() && !confirm('Discard unsaved changes?')) { event.preventDefault(); return; }
  document.querySelector('.edp-main')?.classList.add('navigation-loading');
});
document.addEventListener('htmx:afterRequest', event => {
  document.querySelector('.edp-main')?.classList.remove('navigation-loading');
  if (!event.detail.boosted || !event.detail.failed) return;
  let message = 'Navigation failed. Check your connection and try again.';
  try { message = JSON.parse(event.detail.xhr.responseText).error || message; } catch {}
  window.edpNotice(message);
});
document.addEventListener('htmx:beforeCleanupElement', event => {
  if (event.detail.elt.id === 'page-content') pageLifetime?.abort();
});
document.addEventListener('htmx:afterSettle', event => {
  if (!event.detail.requestConfig?.boosted) return;
  const heading = document.querySelector('#page-content h1');
  if (heading) { heading.tabIndex = -1; heading.focus({preventScroll:true}); }
});
document.addEventListener('htmx:load', activatePage);
document.addEventListener('htmx:historyRestore', () => {
  const page = document.getElementById('page-content');
  if (page) delete page.dataset.initialized;
  activatePage();
});
activatePage();

// Read workbook tabs before importing, without uploading recipients to a campaign.
const workbookReads = new WeakMap();
document.addEventListener('change', async event => {
 const input=event.target;
 if (!(input instanceof HTMLInputElement) || !input.dataset.workbookUrl) return;
 const form=input.closest('form'), label=form.querySelector('[data-workbook-tab]');
 const select=label.querySelector('select'), status=label.querySelector('[data-workbook-status]');
 workbookReads.get(input)?.abort(); select.replaceChildren();
 if (!input.files[0]?.name.toLowerCase().endsWith('.xlsx')) {label.hidden=true;return;}
 label.hidden=false; status.textContent='Reading workbook tabs…';
 const controller=new AbortController();workbookReads.set(input,controller);
 pageLifetime?.signal.addEventListener('abort',()=>controller.abort(),{once:true});
 const fields=new FormData();fields.set('file',input.files[0]);fields.set('csrf',form.querySelector('[name="csrf"]').value);
 try {const response=await fetch(input.dataset.workbookUrl,{method:'POST',body:fields,signal:controller.signal});const data=await response.json();if(!response.ok)throw new Error(data.error||'Could not read workbook');for(const name of data.sheets){const option=document.createElement('option');option.value=name;option.textContent=name;select.append(option);}status.textContent='Choose the tab to import.';}
 catch(error){if(!controller.signal.aborted)status.textContent=error.message;}
});
