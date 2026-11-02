import fs from 'node:fs';
import {spawnSync} from 'node:child_process';
const result={};
fs.mkdirSync('dist',{recursive:true});
for(const name of ['control','gateway','app-shell','grafana']) {
  const source=`ghcr.io/fanhuadesenlinnn/monitor-platform-${name}:1.7.3`;
  const r=spawnSync(process.execPath,['.tools/node_modules/@lazycatcloud/lzc-cli/scripts/cli.js','appstore','copy-image',source,'--arch','amd64','--trace-level','quiet'],{encoding:'utf8',timeout:20*60*1000,maxBuffer:4*1024*1024});
  const refs=(r.stdout||'').split(/\r?\n/).map(s=>s.trim()).filter(s=>/^registry\.lazycat\.cloud\/[A-Za-z0-9_.\/-]+:[A-Za-z0-9_.-]+$/.test(s));
  if(r.status!==0 || refs.length!==1) throw new Error('Transfer failed: '+name);
  if(!refs[0].includes('/monitor-platform-'+name+':')) throw new Error('Unexpected registry name: '+name);
  result[name]={source,image:refs[0],arch:'amd64'};
  fs.writeFileSync('dist/monitor-platform-images.json',JSON.stringify(result,null,2)+'\n');
  console.log(name+': '+refs[0]);
}
