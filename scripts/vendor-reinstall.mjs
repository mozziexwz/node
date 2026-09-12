import fs from 'node:fs/promises';
import crypto from 'node:crypto';
const api = await fetch('https://api.github.com/repos/bin456789/reinstall/commits/main', {headers:{'User-Agent':'MSBOOST-source-audit'}});
if(!api.ok) throw new Error(`GitHub commit lookup: ${api.status}`);
const {sha} = await api.json();
if(!/^[a-f0-9]{40}$/.test(sha)) throw new Error('Invalid upstream commit');
const base=`https://raw.githubusercontent.com/bin456789/reinstall/${sha}`;
const scriptResponse=await fetch(`${base}/reinstall.sh`);
if(!scriptResponse.ok) throw new Error(`Source download: ${scriptResponse.status}`);
const source=await scriptResponse.text();
if(!source.startsWith('#!/usr/bin/env sh')) throw new Error('Unexpected reinstall source');
const pinned=source.replaceAll('bin456789/reinstall/main','bin456789/reinstall/'+sha).replaceAll('bin456789/reinstall@main','bin456789/reinstall@'+sha).replace(/^confhome_cn=.*$/m,`confhome_cn=${base}`);
await fs.mkdir('installers/reinstall',{recursive:true});
await fs.writeFile('installers/reinstall/reinstall.upstream.sh',source);
await fs.writeFile('installers/reinstall/reinstall.sh',pinned);
const manifest={repository:'https://github.com/bin456789/reinstall',commit:sha,sourceURL:`${base}/reinstall.sh`,sourceSHA256:crypto.createHash('sha256').update(source).digest('hex'),executionSHA256:crypto.createHash('sha256').update(pinned).digest('hex'),patch:'Pin internal reinstall resource URLs from main to the same immutable upstream commit. No execution of upstream code during import.'};
for(const filename of ['LICENSE','LICENSE.txt']){const response=await fetch(`${base}/${filename}`);if(response.ok){await fs.writeFile('installers/reinstall/LICENSE',await response.text());manifest.licenseSource=`${base}/${filename}`;break;}}
await fs.writeFile('installers/reinstall/manifest.json',JSON.stringify(manifest,null,2)+'\n');
console.log(JSON.stringify(manifest,null,2));
