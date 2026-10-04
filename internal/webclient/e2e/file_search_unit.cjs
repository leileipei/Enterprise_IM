'use strict';const assert=require('node:assert/strict'),{environment}=require('./file_unit_helpers.cjs');
const env=environment('file-search',['message-search','cross-message-search']),{window,document}=env;const el=id=>document.getElementById('message-search-'+id);
const conv='11111111-1111-4111-8111-111111111111',id='22222222-2222-4222-8222-222222222222';let ctx={identityKey:'actor',membershipId:id,identityEpoch:1,conversation:conv,conversationEpoch:1,kind:'direct',filenameSearchEnabled:true},calls=[],page={matches:[],has_more:true,next_cursor:'cursor-one'},hold=null;
const request=async(p,o)=>{calls.push({path:p,options:o});return hold?await new Promise(resolve=>hold=resolve):page};
const text=new window.MessageSearch(request,()=>({...ctx}),()=>true,()=>true);const files=new window.FileSearch({request,context:()=>({...ctx}),scope:'conversation',download:{request:async()=>{}},openConversation:async()=>{}});text.fileSearch=files;text.open();
const match=()=>({conversation_id:conv,conversation_kind:'direct',message_id:id,seq:'1',sender_user_id:id,server_time:new Date().toISOString(),file_id:id,original_filename:'报告%_.txt',actual_size_bytes:'3',detected_media_type:'text/plain'});
(async()=>{
 text.setMode('file');el('query').value='%_';await text.load(true);assert.equal(calls.length,1);assert(calls[0].path.includes('/files/search?'));assert.equal(new URL(calls[0].path,'https://example.test').searchParams.get('q'),'%_');assert(el('hint').textContent.includes('本页无结果'));assert.equal(el('more').classList.contains('hidden'),false);
 page={matches:[match()],has_more:false,next_cursor:''};await text.load(false);assert(el('list').textContent.includes('报告%_.txt'));assert.equal(calls.length,2);
 for(const bad of [{...match(),actual_size_bytes:'0'},{...match(),message_id:'bad'},{...match(),conversation_kind:'all'},{...match(),caption:'private'}]){page={matches:[bad],has_more:false,next_cursor:''};await text.load(true);assert.equal(el('list').textContent,'')}
 page={matches:Array(21).fill(match()),has_more:false,next_cursor:''};await text.load(true);assert.equal(el('list').textContent,'');
 for(const q of ['x','x'.repeat(101)]){const n=calls.length;el('query').value=q;await text.load(true);assert.equal(calls.length,n)}
 el('query').value='\u0085报告\u0085';page={matches:[{...match(),original_filename:'报告.txt'}],has_more:false,next_cursor:''};await text.load(true);assert.equal(new URL(calls.at(-1).path,'https://example.test').searchParams.get('q'),'报告');
 el('query').value='报告';page={matches:[{...match(),original_filename:'\uFEFF报告.txt'}],has_more:false,next_cursor:''};await text.load(true);assert(el('list').textContent.includes('\uFEFF报告.txt'),'legal FEFF result must not invalidate page');
 el('query').value='报告';hold=true;const late=text.load(true);await env.flush();const signal=calls.at(-1).options.signal;text.setMode('text');assert.equal(signal.aborted,true);hold(page);hold=null;await late;assert.equal(el('list').textContent,'');
 page={conversation_id:conv,messages:[],has_more:false,next_cursor:''};await text.load(true);assert(calls.at(-1).path.includes('/messages/search?'));assert(!calls.at(-1).path.includes('/files/'));
 text.setMode('file');page={matches:[],has_more:true,next_cursor:'same'};await text.load(true);await text.load(false);assert.equal(el('list').textContent,'');assert(el('hint').textContent.includes('失败'));
 console.log('fileSearchModeIsolation, fileSearchSchema, fileSearchEmptyPage, fileSearchContextLateResult, fileSearchUnicodeLiteral: PASS');
})().catch(e=>{console.error(e);process.exitCode=1});
