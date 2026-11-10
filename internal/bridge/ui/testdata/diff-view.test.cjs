'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const view = require('../assets/diff-view.js');
const base = '标题😀\r\n第一行\n第二行\n末行';
const local = '标题😀\r\n第一行\n本地插入\n第二行\n末行';
const remote = '标题😀\r\n第一行\n云端改写\n末行';
const hunk = {id:0,BaseStart:2,BaseEnd:3,LocalStart:2,LocalEnd:4,RemoteStart:2,RemoteEnd:3,Base:'第二行\n',Local:'本地插入\n第二行\n',Remote:'云端改写\n',BaseChanges:[{Start:2,End:2,Kind:'insert',Side:'local'},{Start:2,End:3,Kind:'replace',Side:'remote'}],LocalChanges:[{Start:2,End:3,Kind:'insert',BaseStart:2,BaseEnd:2}],RemoteChanges:[{Start:2,End:3,Kind:'replace',BaseStart:2,BaseEnd:3}]};
test('source rows preserve exact UTF-8, CRLF, LF and missing final newline', () => {
 for (const [side,text] of [['Base',base],['Local',local],['Remote',remote]]) {
   const rows = view.makeLineRows(text,[hunk],side);
   assert.deepEqual(Buffer.from(rows.filter(r=>r.type==='line').map(r=>r.text).join('')),Buffer.from(text));
   assert.deepEqual(rows.filter(r=>r.type==='line').map(r=>r.number),Array.from({length:view.splitLines(text).length},(_,i)=>i+1));
 }
});
test('only changed local line is highlighted, adjacent unchanged context stays plain', () => {
 const rows=view.makeLineRows(local,[hunk],'Local').filter(r=>r.type==='line');
 assert.equal(rows[2].changed,true);assert.equal(rows[2].text,'本地插入\n');
 assert.equal(rows[3].changed,false);assert.deepEqual(rows[3].hunks,[0]);
});
test('insertions and deletions have visible zero-width anchors on the missing side', () => {
 const baseRows=view.makeLineRows(base,[hunk],'Base');assert.ok(baseRows.some(r=>r.type==='anchor'&&r.index===2&&r.label.includes('本地')));
 const deletion={id:1,BaseStart:1,BaseEnd:2,LocalStart:1,LocalEnd:1,LocalChanges:[{Start:1,End:1,BaseStart:1,BaseEnd:2,Kind:'delete'}]};
 const rows=view.makeLineRows('a\n',[deletion],'Local');assert.ok(rows.some(r=>r.type==='anchor'&&r.label.includes('删除')));
});
test('choosing remote hunk preserves untouched Unicode and original newlines', () => {
 assert.equal(view.mergeChoices(base,[hunk],new Map([[0,'Remote']])),remote);
 assert.equal(view.mergeChoices(base,[hunk],new Map([[0,'Local']])),local);
});
test('snippets stay plain text and truncate at Unicode codepoints', () => {
 assert.equal(view.snippet('<img onerror=alert(1)>\n😀中文',3,100),'<img onerror=alert(1)>\n😀中文');
 assert.equal(view.snippet('😀😀😀',2,2),'😀😀…');
});
