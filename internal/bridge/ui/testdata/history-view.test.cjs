'use strict';
const {test}=require('node:test');const assert=require('node:assert/strict');const {restoreRequest}=require('../assets/history-view.js');
test('restore uses the observed current identity and generation, never the historical generation',()=>{
 const detail={restore_available:true,version:{id:'old:1',bodies:{local:{}},event:{profile_id:'account',mount_id:'mount',entry:{id:'file',generation:1,remote:'old-name.md'}}},current:{entry:{id:'file',generation:9,remote:'new-name.md'},deleted:false,parent:{id:'parent',generation:4}}};
 const r=restoreRequest(detail,'request-id');assert.equal(r.expected_generation,9);assert.equal(r.expected_path,'new-name.md');assert.equal(r.parent_generation,4);assert.equal(r.version_id,'old:1');assert.equal(r.request_id,'request-id');
 detail.current.entry.id='different-file';assert.throws(()=>restoreRequest(detail,'next'));
});
