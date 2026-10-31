import type { APIRequestContext } from 'playwright';
import { probeScopedSibling } from '../helpers/tunnelView';
const origin = 'https://q-' + 'a'.repeat(52) + '.qurl.site';
const own = origin + '/views/' + '1'.repeat(32);
const sibling = '/views/' + '2'.repeat(32);
function client(statuses: number[], body = 'Resource not found\n') {
  const dispose = jest.fn().mockResolvedValue(undefined);
  const get = jest.fn();
  for (const status of statuses) get.mockResolvedValueOnce({status:()=>status,dispose,body:async()=>Buffer.from(body),headers:()=>({'cache-control':'no-store'})});
  return {get,dispose,request:{get} as unknown as APIRequestContext};
}
test('records converged denial only as pending internal403 evidence',async()=>{
 const c=client([200,404,200]);const result=await probeScopedSibling(c.request,own,sibling);
 expect(result).toMatchObject({ownBefore:200,siblingWireStatus:404,ownAfter:200,siblingWireOutcome:'CONVERGED_NOT_FOUND_PENDING_INTERNAL_403'});
 expect(c.get.mock.calls.map(x=>x[0])).toEqual([own,origin+sibling,own]);
 expect(c.get.mock.calls.every(x=>x[1].maxRedirects===0 && x[1].maxRetries===0)).toBe(true);
 const ids=c.get.mock.calls.map(x=>x[1].headers['X-Request-ID']);expect(new Set(ids).size).toBe(3);expect(ids).toEqual([result.requestIDs.ownBefore,result.requestIDs.sibling,result.requestIDs.ownAfter]);
});
test.each([403,503,200,302])('HTTP%s is not the converged denial',async status=>{
 await expect(probeScopedSibling(client([200,status,200]).request,own,sibling)).rejects.toThrow(/qualified router denial/);
});
test('arbitrary404 body is not the converged denial',async()=>{
 await expect(probeScopedSibling(client([200,404,200],'WAF denied').request,own,sibling)).rejects.toThrow(/qualified router denial/);
});
test.each(['timeout','ENOTFOUND','secret'])('unqualified transport error never passes',async message=>{
 const c=client([200]);c.get.mockRejectedValueOnce(new Error(message));await expect(probeScopedSibling(c.request,own,sibling)).rejects.toThrow();
});
test('exact connection-close class still records pending internal proof only',async()=>{
 const c=client([]);c.get.mockResolvedValueOnce({status:()=>200,dispose:async()=>{}}).mockRejectedValueOnce(new Error('socket hang up')).mockResolvedValueOnce({status:()=>200,dispose:async()=>{}});
 const result=await probeScopedSibling(c.request,own,sibling);expect(result.siblingWireStatus).toBeNull();expect(result.siblingWireOutcome).toBe('CONNECTION_CLOSED_PENDING_INTERNAL_403');
});
test('own control failure is not scope proof',async()=>{await expect(probeScopedSibling(client([403]).request,own,sibling)).rejects.toThrow(/own controls/)});
test.each(['https://elsewhere.example/','/views/'+ '1'.repeat(32),'/views/../sibling','/views/'+ '2'.repeat(32)+'?token=bad'])('invalid target fails before request',async path=>{const c=client([]);await expect(probeScopedSibling(c.request,own,path)).rejects.toThrow(/canonical/);expect(c.get).not.toHaveBeenCalled()});
test('untrusted origin fails before request',async()=>{const c=client([]);await expect(probeScopedSibling(c.request,own.replace('.site','.site.evil.example'),sibling)).rejects.toThrow(/canonical/);expect(c.get).not.toHaveBeenCalled()});

// Receiving headers makes a later body reset a failed response, not a denial.
test.each([200, 403, 404, 503])('HTTP%s with a body reset cannot pass', async status => {
  const c = client([]);
  c.get.mockResolvedValueOnce({ status: () => 200, dispose: async () => {} });
  c.get.mockResolvedValueOnce({
    status: () => status,
    body: async () => { throw new Error('socket hang up'); },
    headers: () => ({ 'cache-control': 'no-store' }),
    dispose: async () => {},
  });
  await expect(probeScopedSibling(c.request, own, sibling)).rejects.toThrow(/qualified router denial/);
  expect(c.get).toHaveBeenCalledTimes(2);
});
