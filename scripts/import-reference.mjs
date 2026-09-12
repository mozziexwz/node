// Reproducible, byte-preserving import of the user's approved references.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
const [ui, installer] = process.argv.slice(2);
if (!ui || !installer) throw new Error('Usage: node scripts/import-reference.mjs UI_FOLDER INSTALLER');
fs.mkdirSync('docs/requirements', {recursive:true});
fs.mkdirSync('installers/node', {recursive:true});
fs.mkdirSync('apps/web/src', {recursive:true});
fs.copyFileSync(path.join(ui,'MSBOOST-产品规划-v4.1.md'),'docs/requirements/product-v4.1.md');
fs.copyFileSync(installer,'installers/node/msboost.sh');
const html=fs.readFileSync(path.join(ui,'01-public.html'),'utf8');
const css=html.match(/<style>([\s\S]*?)<\/style>/)?.[1];
if (!css) throw new Error('Approved stylesheet not found');
fs.writeFileSync('apps/web/src/approved.css',css);
const manifest=[['product-v4.1.md','docs/requirements/product-v4.1.md'],['msboost.sh','installers/node/msboost.sh']].map(([name,file])=>({name,sha256:crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex')}));
fs.writeFileSync('docs/requirements/source-manifest.json',JSON.stringify(manifest,null,2)+'\n');
console.log('Imported approved stylesheet, specification and byte-identical node installer.');
