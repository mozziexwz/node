import {test} from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import ts from 'typescript';
const output=ts.transpileModule(fs.readFileSync(new URL('../src/navigation.ts',import.meta.url),'utf8'),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText;
const {navigationGroups}=await import('data:text/javascript;base64,'+Buffer.from(output).toString('base64'));
for(const [role,admin,subscribed] of [['admin',true,false],['member',false,false],['subscriber',false,true]]) {
  test(`navigation ${role}: free tools precede paid business, no duplicates`,()=>{
    const groups=navigationGroups(admin,subscribed),items=groups.flatMap(g=>g.items);
    assert.equal(items[0],'tutorials');
    assert.equal(new Set(items).size,items.length);
    assert.deepEqual(groups.find(g=>g.title==='免费部署工具').items,['deploy','relay','dd','tasks']);
    assert.ok(items.indexOf('dd')<items.indexOf('plans'));
    assert.ok(items.includes('account')&&items.includes('tickets'));
    if(admin) assert.ok(items.includes('users')&&items.includes('executors')&&items.includes('rules'));
    else {assert.ok(!items.includes('users'));assert.equal(items.includes('routes'),subscribed);}
  });
}
test('favicon and Maple share the outlined leaf and semicircle',()=>{
  const favicon=fs.readFileSync(new URL('../public/favicon.svg',import.meta.url),'utf8');
  const ui=fs.readFileSync(new URL('../src/ui.tsx',import.meta.url),'utf8');
  const maple=ui.slice(ui.indexOf('export function Maple()'),ui.indexOf('export function Brand()'));
  const paths=source=>[...source.matchAll(/<path\s+([\s\S]*?)\/>/g)].map(([,attributes])=>
    Object.fromEntries([...attributes.matchAll(/([\w-]+)="([^"]*)"/g)].map(([,key,value])=>[
      key.replace(/[A-Z]/g,char=>'-'+char.toLowerCase()),value,
    ])));
  assert.deepEqual(paths(favicon),paths(maple));
  assert.equal(paths(favicon).length,2);
  assert.equal(paths(favicon)[0].d,'M5 31a27 27 0 0 0 54 0');
  assert.equal(paths(favicon)[1].fill,'#ffffff');
  for(const path of paths(favicon))assert.equal(path.stroke,'#000000');
  for(const source of [favicon,maple])assert.match(source,/viewBox="0 0 64 64"/);
  assert.match(favicon,/枫叶/);
  assert.doesNotMatch(favicon,/<script|<image|href=/i);
});
