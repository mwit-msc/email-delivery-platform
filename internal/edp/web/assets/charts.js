'use strict';
window.edpInitCharts = async function(panel, signal) {
  let activity, applications, outcomes;
  Chart.defaults.color = '#a5a5ad';
  Chart.defaults.borderColor = '#353535';
  Chart.defaults.font.family = 'Outfit, Noto Sans Thai, sans-serif';
  const options = {responsive:true,maintainAspectRatio:false,animation:!matchMedia('(prefers-reduced-motion: reduce)').matches,plugins:{legend:{position:'bottom'}},scales:{y:{beginAtZero:true,ticks:{precision:0}}}};
  const page = panel.closest('#page-content');
  async function refresh() {
    try {
      const res = await fetch(panel.dataset.statsUrl, {signal});
      if (!res.ok) throw new Error('Statistics unavailable');
      const data = await res.json();
      if (signal.aborted || !panel.isConnected) return;
      for (const [key,value] of Object.entries(data.totals)) page.querySelectorAll('[data-stat="'+key+'"]').forEach(el => {el.textContent=typeof value==='number'?new Intl.NumberFormat('en',{maximumFractionDigits:1}).format(value):value;});
      const rows = data.series;
      const chartData = {labels:rows.map(r=>r.label),datasets:[{label:'Submitted',data:rows.map(r=>r.submitted),borderColor:'#9d8df5',backgroundColor:'#9d8df518',tension:.25},{label:'Gateway accepted',data:rows.map(r=>r.accepted),borderColor:'#5e5ce6',tension:.25},{label:'Failed / bounced',data:rows.map(r=>r.failed),borderColor:'#efa3ad',tension:.25}]};
      if (activity) {activity.data=chartData;activity.update('none');}
      else activity = new Chart(page.querySelector('#activity-chart'),{type:'line',data:chartData,options});
      const appData = {labels:data.applications.map(a=>a.name),datasets:[{label:'Messages',data:data.applications.map(a=>a.total),backgroundColor:'#9d8df5',borderRadius:3}]};
      if (applications) {applications.data=appData;applications.update('none');}
      else applications = new Chart(page.querySelector('#application-chart'),{type:'bar',data:appData,options:{...options,indexAxis:'y',plugins:{legend:{display:false}}}});
      const outcomeData = {labels:['Gateway accepted','Waiting','Failed','Bounced','Unknown'],datasets:[{data:[data.totals.accepted,data.totals.queued,data.totals.failed,data.totals.bounced,data.totals.unknown],backgroundColor:['#5e5ce6','#9584ed','#efa3ad','#ff9900','#6d6d78'],borderWidth:0,spacing:4,borderRadius:5}]};
      if (outcomes) {outcomes.data=outcomeData;outcomes.update('none');}
      else outcomes = new Chart(page.querySelector('#outcome-chart'),{type:'doughnut',data:outcomeData,options:{responsive:true,maintainAspectRatio:false,cutout:'76%',plugins:{legend:{position:'bottom',labels:{boxWidth:10,padding:16}}}}});
      page.querySelector('#stats-updated').textContent='Updated '+new Date().toLocaleTimeString('en',{timeZone:'Asia/Bangkok'});
    } catch (error) {
      if (!signal.aborted && panel.isConnected) page.querySelector('#stats-updated').textContent='Updates unavailable; showing the last loaded data.';
    }
  }
  await refresh();
  if (signal.aborted) return;
  const timer = setInterval(() => {if (!document.hidden) refresh();},10000);
  signal.addEventListener('abort', () => {clearInterval(timer);[activity,applications,outcomes].forEach(chart=>chart?.destroy());}, {once:true});
};
