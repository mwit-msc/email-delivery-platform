'use strict';
window.edpInitSheet = async function(root, signal) {
  const find = selector => root.querySelector(selector);
  const status = find('[data-sheet-status]');
  const listen = (element, name, fn) => element?.addEventListener(name, fn, {signal});
  const page = root.closest('#page-content');
  const responseJSON = async response => {
    if(response.status===401||(response.redirected&&new URL(response.url).pathname==='/login')){location.assign('/login');throw new Error('Session expired. Sign in again.');}
    return response.json();
  };
  let rows=[], links={}, mapping={}, revision='', dirty=false, mappingDirty=false, busy=false, polling=false, progress={}, offset=0, reviewOffset=0;
  const tabs=Array.from(page.querySelectorAll('[data-sheet-tab]'));
  const setTab = name => {
    for(const tab of tabs){const active=tab.dataset.sheetTab===name;tab.setAttribute('aria-selected',String(active));tab.tabIndex=active?0:-1;tab.classList.toggle('active',active);tab.classList.toggle('btn-primary',active);tab.classList.toggle('btn-outline-secondary',!active);}
    find('[data-sheet-data]').hidden=name!=='data';
    if(find('[data-sheet-review]'))find('[data-sheet-review]').hidden=name!=='review';
    if(page.querySelector('#campaign-mapping'))page.querySelector('#campaign-mapping').hidden=name!=='mapping';
    root.hidden=name==='mapping';
  };
  for(const tab of tabs){listenTab(tab);}
  function listenTab(tab){tab.addEventListener('click',()=>setTab(tab.dataset.sheetTab),{signal});tab.addEventListener('keydown',event=>{let i=tabs.indexOf(tab);if(event.key==='ArrowRight')i=(i+1)%tabs.length;else if(event.key==='ArrowLeft')i=(i+tabs.length-1)%tabs.length;else return;event.preventDefault();tabs[i].focus();setTab(tabs[i].dataset.sheetTab);},{signal});}
  if(tabs.length){const requested=new URLSearchParams(location.search).get('tab')||(new URLSearchParams(location.search).has('template')?'mapping':'data');setTab(tabs.some(tab=>tab.dataset.sheetTab===requested)?requested:'data');}
  const size=50;
  const element = (tag, text, cls) => {const el=document.createElement(tag);if(text!==undefined)el.textContent=text;if(cls)el.className=cls;return el;};
  const button = (text, fn, cls='btn btn-sm btn-outline-secondary') => {const el=element('button',text,cls);el.type='button';el.addEventListener('click',fn,{signal});return el;};
  const mapForm = page.querySelector('#campaign-mapping form input[name="action"][value="map"]')?.form;
  const mapSelects = Array.from(mapForm?.querySelectorAll('select') || []).filter(el=>/^(email|school|event|award|url|var\.)/.test(el.name));
  const setDirty = () => {dirty=true;find('[data-sheet-save]').disabled=false;status.textContent='Unsaved sheet changes. Save before matching or validating the email.';};
  window.edpPageDirty=()=>dirty||mappingDirty||busy;
  const warn=event=>{if(dirty||mappingDirty||busy){event.preventDefault();event.returnValue='';}};
  window.addEventListener('beforeunload',warn);
  signal.addEventListener('abort',()=>{window.removeEventListener('beforeunload',warn);window.edpPageDirty=null;},{once:true});
  const labelFor = select => select.closest('label')?.firstChild?.textContent.trim() || select.name;
  const samples = () => {
    for(const select of mapSelects) {
      let sample=select.closest('label').querySelector('.sheet-sample');
      if(!sample){sample=element('small','','sheet-sample');select.closest('label').append(sample);}
      const column=Number(select.value);
      const value=rows.slice(1).map(row=>row[column]||'').find(value=>value.trim());
      sample.textContent=column<0?'Use the default or constant below.':('Example: '+(value||'empty column'));
    }
  };
  const templateFields = () => {
    if(root.dataset.kind!=='certificate')return;
    const templates=JSON.parse(root.dataset.templates||'[]');
    const combined=mapForm.querySelector('[name="combine_recipients"]')?.checked;
    const chosen=combined?[mapForm.querySelector('[name="combined_template"]')?.value]:Array.from(mapForm.querySelectorAll('select[name^="rule."]'),select=>select.value);
    const names=new Set(templates.filter(t=>chosen.includes(t.id)||chosen.includes(t.slug)).flatMap(t=>t.variables||[]));
    for(const label of mapForm.querySelectorAll('[data-template-variable]')) {
      label.hidden=!names.has(label.dataset.templateVariable);
      for(const field of label.querySelectorAll('input,select'))field.disabled=label.hidden;
    }
    const heading=mapForm.querySelector('[data-template-fields]');if(heading)heading.hidden=names.size===0;
    const target=find('[data-map-target]'), selected=target.value;
    target.replaceChildren(new Option('Choose an email field…',''),...mapSelects.filter(select=>!select.disabled).map(select=>new Option(labelFor(select),select.name)));
    if(Array.from(target.options).some(option=>option.value===selected))target.value=selected;
  };
  const fillSelect = (select, includeDefault=false) => {
    const value=select.value;
    const options=[];
    if(includeDefault)options.push(new Option('Use default / constant','-1'));
    for(const [i,name] of (rows[0]||[]).entries())options.push(new Option(String.fromCharCode(65+Math.floor(i/26)-1).replace('@','')+String.fromCharCode(65+i%26)+' · '+(name||'Unnamed column'),String(i)));
    select.replaceChildren(...options);if(Array.from(select.options).some(option=>option.value===value))select.value=value;
  };
  const refreshColumns = () => {
    for(const select of mapSelects)fillSelect(select,['event','award'].includes(select.name)||select.name.startsWith('var.'));
    const identity=find('[name="columns"]'), url=find('[name="url_column"]');
    if(identity){const previous=Array.from(identity.selectedOptions,option=>option.value);fillSelect(identity);for(const option of identity.options)option.selected=previous.includes(option.value)||links.columns?.includes(Number(option.value));}
    if(url){fillSelect(url);const guessed=rows[0]?.findIndex(name=>/certificate.*url|certificate.*link|^url$|link/i.test(name));if(Number.isInteger(links.url_column)&&links.matches?.length)url.value=String(links.url_column);else if(guessed>=0)url.value=String(guessed);}
    const fieldset=mapForm?.querySelector('fieldset');
    if(fieldset){const checked=new Set(Array.from(fieldset.querySelectorAll('[name="name_columns"]:checked'),el=>el.value));fieldset.querySelectorAll('label').forEach(el=>el.remove());for(const [i,name] of rows[0].entries()){const label=element('label',undefined,'form-check'),input=element('input',undefined,'form-check-input');input.type='checkbox';input.name='name_columns';input.value=String(i);input.checked=checked.has(String(i));label.append(input,document.createTextNode(name||'Column '+(i+1)));fieldset.append(label);}}
    samples();templateFields();
  };
  const renderGrid = () => {
    offset=Math.min(offset,Math.max(0,Math.floor((rows.length-2)/size)*size));
    const table=element('table',undefined,'recipient-grid'), head=element('thead'), header=element('tr');
    header.append(element('th','Row','sheet-row-number'));
    for(const [c,name] of (rows[0]||[]).entries()){
      const th=element('th'), wrap=element('div',undefined,'sheet-column-heading');
      const input=element('input',undefined,'sheet-cell');input.value=name;input.setAttribute('aria-label','Column '+(c+1)+' name');
      listen(input,'input',()=>{rows[0][c]=input.value;setDirty();});listen(input,'change',refreshColumns);
      const connect=button('Link',()=>{
        const target=mapSelects.find(select=>select.name===find('[data-map-target]').value);
        if(!target){status.textContent='Choose an email field above, then click Link on its source column.';find('[data-map-target]').focus();return;}
        target.value=String(c);mappingDirty=true;target.classList.add('mapping-connected');target.dispatchEvent(new Event('change'));status.textContent=labelFor(target)+' uses '+(rows[0][c]||'column '+(c+1))+'. Validate the mapping to save it.';
      },'');connect.setAttribute('aria-label','Connect column '+(c+1)+' to selected email field');
      wrap.append(input,connect);th.append(wrap);header.append(th);
    }
    head.append(header);table.append(head);const body=element('tbody');
    for(let r=offset+1;r<Math.min(rows.length,offset+size+1);r++){
      const tr=element('tr');tr.append(element('th',String(r+1),'sheet-row-number'));
      for(let c=0;c<rows[0].length;c++){
        const td=element('td'), input=element('input',undefined,'sheet-cell');input.value=rows[r][c]||'';input.dataset.row=r;input.dataset.col=c;
        input.setAttribute('aria-label','Row '+(r+1)+', '+(rows[0][c]||'column '+(c+1)));
        listen(input,'input',()=>{while(rows[r].length<=c)rows[r].push('');rows[r][c]=input.value;setDirty();samples();});
        listen(input,'keydown',event=>{
          let rr=r,cc=c;
          if(event.key==='ArrowUp')rr--;else if(event.key==='ArrowDown'||event.key==='Enter')rr++;else return;
          const next=table.querySelector(`[data-row="${rr}"][data-col="${cc}"]`);if(next){event.preventDefault();next.focus();}
        });
        listen(input,'paste',event=>{
          const text=event.clipboardData.getData('text/plain');if(!/[\t\n]/.test(text))return;
          event.preventDefault();const block=text.replace(/\r\n?/g,'\n').replace(/\n$/,'').split('\n').map(line=>line.split('\t'));
          if(r+block.length>5001||c+Math.max(...block.map(row=>row.length))>52){status.textContent='Paste exceeds 5,000 rows or 52 columns.';return;}
          const width=c+Math.max(...block.map(row=>row.length));while(rows[0].length<width)rows[0].push('Column '+(rows[0].length+1));
          for(const [dr,values] of block.entries()){while(rows.length<=r+dr)rows.push([]);for(const [dc,value] of values.entries()){while(rows[r+dr].length<=c+dc)rows[r+dr].push('');rows[r+dr][c+dc]=value;}}
          setDirty();refreshColumns();renderGrid();find(`[data-row="${r}"][data-col="${c}"]`)?.focus();
        });
        td.append(input);tr.append(td);
      }
      body.append(tr);
    }
    table.append(body);find('[data-sheet-grid]').replaceChildren(table);
    find('[data-sheet-page]').textContent=(rows.length>1?'Rows '+(offset+2)+'–'+Math.min(rows.length,offset+size+1)+' of '+(rows.length-1)+' recipients':'No rows');
    find('[data-sheet-prev]').disabled=offset===0;find('[data-sheet-next]').disabled=offset+size>=rows.length-1;
  };
  const filteredMatches = () => (links.matches||[]).filter(m=>{
    const filter=find('[data-review-filter]').value;
    return filter==='all'||(filter==='ambiguous'?m.ambiguous&&m.status==='pending':m.status===filter);
  });
  const renderReview = () => {
    const area=find('[data-link-review]');if(!area)return;
    area.hidden=!links.matches?.length;if(area.hidden)return;
    const filtered=filteredMatches();reviewOffset=Math.min(reviewOffset,Math.max(0,Math.floor((filtered.length-1)/size)*size));
    const known=new Map((links.files||[]).map(file=>[file.id,file]));const body=find('[data-review-rows]');body.replaceChildren();
    const pending=links.matches.filter(m=>m.status==='pending').length;
    find('[data-review-count]').textContent=pending+' need review · '+links.matches.filter(m=>m.status==='accepted').length+' accepted';
    for(const m of filtered.slice(reviewOffset,reviewOffset+size)){
      const tr=element('tr');tr.append(element('td',String(m.row)));
      tr.append(element('td',(links.columns||[]).map(c=>rows[m.row-1]?.[c]).filter(Boolean).join(' / '),'certificate-identity'));
      const fileCell=element('td'), evidence=element('td',undefined,'certificate-evidence'), actions=element('td');
      if(m.status==='existing'){fileCell.textContent='Existing link retained';evidence.textContent=m.reason;actions.textContent='Already linked';}
      else {
        const picker=element('select',undefined,'form-select certificate-picker');picker.setAttribute('aria-label','PDF for source row '+m.row);picker.append(new Option('Choose a PDF…',''));
        // Options are created on focus to keep large folders lightweight.
        if(m.file_id&&known.has(m.file_id))picker.append(new Option(known.get(m.file_id).name,m.file_id));picker.value=m.file_id||'';
        listen(picker,'focus',()=>{if(picker.dataset.loaded)return;const selected=picker.value;picker.replaceChildren(new Option('Choose a PDF…',''),...links.files.map(f=>new Option(f.name+' · '+f.id.slice(-6),f.id)));picker.value=selected;picker.dataset.loaded='true';});
        const open=element('a','Open PDF','d-block small mt-2');open.target='_blank';open.rel='noopener noreferrer';
        const updateOpen=()=>{open.hidden=!picker.value;open.href=picker.value?'https://drive.google.com/file/d/'+encodeURIComponent(picker.value)+'/view':'#';};updateOpen();listen(picker,'change',updateOpen);fileCell.append(picker,open);
        const details=element('details'),summary=element('summary','Read extracted text');details.append(summary);
        const extracted=element('pre','','certificate-text-evidence');details.append(extracted);
        listen(details,'toggle',async()=>{if(!details.open)return;const file=known.get(picker.value);if(!file){extracted.textContent='Choose a PDF first.';return;}extracted.textContent='Loading locally extracted text…';try{const response=await fetch(root.dataset.url.replace(/\/workspace$/,'')+'/certificate-text/'+encodeURIComponent(file.id),{signal});const data=await responseJSON(response);if(!response.ok)throw new Error(data.error||'Extracted text unavailable');if(!signal.aborted)extracted.textContent=({'text':'PDF text','ocr':'Local OCR','text+ocr':'PDF text + local OCR'}[data.method]||data.method)+' · '+data.pages+' page(s)\n\n'+data.text;}catch(error){if(!signal.aborted)extracted.textContent=error.message;}});
        listen(picker,'change',()=>{details.open=false;extracted.textContent='';});fileCell.append(details);
        if(known.get(m.file_id)?.error)evidence.append(element('small',known.get(m.file_id).error,'d-block text-warning mt-2'));
        evidence.append(element('span',m.score?m.score+'%':(m.status==='accepted'?'Manual selection':'No match'),'certificate-score'),element('small',m.reason,'d-block text-secondary mt-1'));
        listen(picker,'change',()=>{const changed=picker.value!==m.file_id;evidence.querySelector('.certificate-score').textContent=changed?'Manual selection':(m.score?m.score+'%':'No match');let warning=evidence.querySelector('[data-choice-warning]');if(!warning){warning=element('small','Review the selected PDF. The automatic score belongs to the original suggestion.','d-block text-warning mt-2');warning.dataset.choiceWarning='true';evidence.append(warning);}warning.hidden=!changed;});
        if(m.evidence)evidence.append(element('small','Matched values: '+m.evidence,'d-block mt-2'));
        if(m.ambiguous)evidence.append(element('span',m.status==='accepted'?'Accepted after manual review':'Manual review required',m.status==='accepted'?'status status-completed mt-2':'status status-unknown mt-2'));
        const group=element('div',undefined,'certificate-review-actions');
        group.append(button(m.status==='accepted'?'Update link':'Accept link',()=>request({action:'review',decisions:[{row:m.row,file_id:picker.value,accept:true}]}),'btn btn-sm btn-outline-primary'),button('Reject',()=>request({action:'review',decisions:[{row:m.row,file_id:'',accept:false}]})));
        for(const control of group.querySelectorAll('button'))control.disabled=links.scan_state==='reading';
        actions.append(element('span',m.status,'status mb-2'),group);
      }
      tr.append(fileCell,evidence,actions);body.append(tr);
    }
    find('[data-review-page]').textContent=filtered.length?'Matches '+(reviewOffset+1)+'–'+Math.min(filtered.length,reviewOffset+size)+' of '+filtered.length:'No matches for this filter';
    find('[data-review-prev]').disabled=reviewOffset===0;find('[data-review-next]').disabled=reviewOffset+size>=filtered.length;
  };
  const renderProgress = () => {
    const box=find('[data-scan-progress]');if(!box)return;
    box.hidden=!links.scan_id;const running=links.scan_state==='reading';
    const total=progress.total||links.files?.length||0, processed=(progress.done||0)+(progress.failed||0);
    find('[data-scan-status]').textContent=running?'Reading PDFs locally: '+processed+' / '+total+' · '+(progress.failed||0)+' failed':'Scan '+links.scan_state+' · '+(progress.done||0)+' read · '+(progress.failed||0)+' unreadable';
    find('[data-scan-bar]').value=running?(total?100*processed/total:0):100;
    find('[data-cancel-scan]').hidden=!running;
    find('[data-auto-accept]').disabled=running;
    for(const input of find('[data-sheet-data]').querySelectorAll('input,button[data-add-row],button[data-add-column]'))input.disabled=running;
  };
  const pollScan = async () => {
    if(signal.aborted||polling||busy||links.scan_state!=='reading')return;polling=true;
    try {
      const response=await fetch(root.dataset.url+'?progress=1',{signal});const data=await responseJSON(response);if(!response.ok)throw new Error(data.error||'Scan progress unavailable');if(signal.aborted)return;
      progress=data.progress;renderProgress();
      if(data.scan_state!=='reading'){
        const full=await fetch(root.dataset.url,{signal});const result=await responseJSON(full);if(!full.ok)throw new Error(result.error||'Could not load completed scan');if(signal.aborted)return;
        rows=result.rows.map(row=>row||[]);links=result.links;revision=result.revision;progress=result.progress;
        refreshColumns();renderGrid();renderReview();renderProgress();status.textContent='PDF scan finished. Review text evidence and any ambiguous matches.';
      }
    }catch(error){if(!signal.aborted)status.textContent=error.message;}finally{polling=false;}
  };
  const timer=setInterval(pollScan,4000);signal.addEventListener('abort',()=>clearInterval(timer),{once:true});
  listen(find('[data-cancel-scan]'),'click',()=>request({action:'cancel_scan'}));
  const refreshMessages = async () => {
    const current=page.querySelector('#campaign-recipient-review');if(!current)return;
    try {
      const response=await fetch(root.dataset.url.replace(/\/workspace$/,''),{signal});
      if(!response.ok)throw new Error('Could not refresh recipient validation');
      const html=await response.text();if(signal.aborted)return;
      const replacement=new DOMParser().parseFromString(html,'text/html').querySelector('#campaign-recipient-review');
      if(replacement&&current.isConnected)current.replaceWith(replacement);
    }catch(error){if(!signal.aborted)status.textContent+=' Reload the page to refresh recipient validation.';}
  };
  const request = async input => {
    if(busy)return;
    if(dirty&&input.action!=='edit'){status.textContent='Save your sheet changes before reviewing links.';return;}
    busy=true;const controls=Array.from(root.querySelectorAll('button,input,select'));const disabled=controls.map(el=>el.disabled);controls.forEach(el=>el.disabled=true);status.textContent=input.action==='match'?'Reading folder and comparing PDF filenames…':'Saving changes…';
    try {
      const response=await fetch(root.dataset.url,{method:'POST',signal,headers:{'Content-Type':'application/json','X-CSRF-Token':root.dataset.csrf},body:JSON.stringify({...input,revision})});
      const data=await responseJSON(response);if(!response.ok)throw new Error(data.error||'Changes could not be saved');if(signal.aborted)return;
      rows=data.rows.map(row=>row||[]);links=data.links;revision=data.revision;dirty=false;
      status.textContent=input.action==='match'?'Local PDF scan queued. You can leave this page and return later.':'Saved. Review recipient messages and validate the email mapping before campaign approval.';
      refreshColumns();renderGrid();renderReview();renderProgress();find('[data-sheet-save]').disabled=true;
      await refreshMessages();
    }catch(error){if(!signal.aborted)status.textContent=error.message;}
    finally{busy=false;if(!signal.aborted){controls.forEach((el,i)=>{if(el.isConnected)el.disabled=disabled[i];});find('[data-sheet-save]').disabled=!dirty;renderProgress();}}
  };
  listen(find('[data-sheet-save]'),'click',()=>request({action:'edit',rows}));
  listen(find('[data-add-row]'),'click',()=>{if(rows.length>=5001){status.textContent='Maximum 5,000 recipient rows.';return;}rows.push([]);offset=Math.floor((rows.length-2)/size)*size;setDirty();renderGrid();});
  listen(find('[data-add-column]'),'click',()=>{if(rows[0].length>=52){status.textContent='Maximum 52 columns.';return;}rows[0].push(root.dataset.kind==='certificate'&&!rows[0].some(name=>/certificate.*url|^url$|link/i.test(name))?'certificate_url':'Column '+(rows[0].length+1));setDirty();refreshColumns();renderGrid();});
  listen(find('[data-sheet-prev]'),'click',()=>{offset=Math.max(0,offset-size);renderGrid();});listen(find('[data-sheet-next]'),'click',()=>{offset+=size;renderGrid();});
  listen(find('[data-review-prev]'),'click',()=>{reviewOffset=Math.max(0,reviewOffset-size);renderReview();});listen(find('[data-review-next]'),'click',()=>{reviewOffset+=size;renderReview();});listen(find('[data-review-filter]'),'change',()=>{reviewOffset=0;renderReview();});
  listen(find('[data-auto-accept]'),'click',()=>request({action:'auto',threshold:Number(find('[data-confidence]').value)}));
  listen(find('[data-match-form]'),'submit',event=>{event.preventDefault();request({action:'match',folder:find('[name="folder"]').value,columns:Array.from(find('[name="columns"]').selectedOptions,option=>Number(option.value)),url_column:Number(find('[name="url_column"]').value),force_ocr:find('[name="force_ocr"]').checked,auto_accept:find('[name="auto_accept"]').checked,threshold:Number(find('[name="scan_threshold"]').value)});});
  // Avoid losing unsaved cells when a normal mutation form navigates away.
  listen(page,'submit',event=>{if(event.target===mapForm){if(dirty){event.preventDefault();status.textContent='Save sheet changes before saving the mapping.';return;}mappingDirty=false;}else if(!root.contains(event.target)&&(dirty||mappingDirty)&&!confirm('Discard unsaved sheet or mapping changes?'))event.preventDefault();});
  listen(mapForm,'input',()=>{mappingDirty=true;});listen(mapForm,'change',()=>{mappingDirty=true;});
  listen(page.querySelector('[data-save-mapping]'),'click',()=>{if(dirty){setTab('data');status.textContent='Save sheet changes before saving the mapping.';return;}mapForm.requestSubmit();});
  for(const select of mapSelects)listen(select,'change',samples);
  for(const field of mapForm?.querySelectorAll('select[name^="rule."],[name="combined_template"],[name="combine_recipients"]')||[])listen(field,'change',templateFields);
  try {
    const response=await fetch(root.dataset.url,{signal});const data=await responseJSON(response);if(!response.ok)throw new Error(data.error||'Could not load sheet');if(signal.aborted)return;
    rows=data.rows.map(row=>row||[]);links=data.links||{};mapping=data.mapping||{};revision=data.revision;progress=data.progress||{};
    for(const select of mapSelects){
      const key=select.name==='url'?'url':select.name;
      const saved=select.name.startsWith('var.')?mapping.columns?.[select.name.slice(4)]:mapping[key];
      if(saved!==undefined)select.value=String(saved);
      else if(root.dataset.kind==='bulk'){const guess=rows[0].findIndex(h=>h.toLowerCase().replace(/\s+/g,'_')===(select.name==='email'?'email':select.name.slice(4)));if(guess>=0)select.value=String(guess);}
    }
    for(const input of mapForm?.querySelectorAll('input[name^="const."]')||[]){input.value=mapping.constants?.[input.name.slice(6)]||'';}
    const target=find('[data-map-target]');for(const select of mapSelects)target.append(new Option(labelFor(select),select.name));
    if(find('[name="folder"]'))find('[name="folder"]').value=links.folder||'';
    refreshColumns();renderGrid();renderReview();renderProgress();status.textContent=(rows.length-1)+' recipients · click a cell to edit';
  }catch(error){if(!signal.aborted)status.textContent=error.message;}
};
