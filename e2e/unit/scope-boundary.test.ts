import type { APIRequestContext } from 'playwright';
import { probeScopedSibling } from '../helpers/tunnelView';
const origin = 'https://q-' + 'a'.repeat(52) + '.qurl.site';
const own = origin + '/views/' + '1'.repeat(32);
const sibling = '/views/' + '2'.repeat(32);
function client(statuses: number[]) {
  const dispose = jest.fn().mockResolvedValue(undefined);
  const get = jest.fn();
  for (const status of statuses) get.mockResolvedValueOnce({ status: () => status, dispose });
  return { get, dispose, request: { get } as unknown as APIRequestContext };
}
test('existing session requests same origin own200 sibling403 own200 with no redirect or knock', async () => {
  const c = client([200,403,200]);
  await expect(probeScopedSibling(c.request, own, sibling)).resolves.toEqual({ownBefore:200,sibling:403,ownAfter:200});
  expect(c.get.mock.calls.map(x=>x[0])).toEqual([own,origin+sibling,own]);
  expect(c.get.mock.calls.every(x=>x[1].maxRedirects===0 && x[1].timeout===10000)).toBe(true);
  expect(c.dispose).toHaveBeenCalledTimes(3);
});
test.each([[403,403,403],[200,404,200],[200,503,200],[200,403,401],[200,200,200]])('rejects ambiguous response sequence %j', async (...statuses: number[]) => {
  await expect(probeScopedSibling(client(statuses).request,own,sibling)).rejects.toThrow(/requires own200 sibling403 own200/);
});
test.each(['https://elsewhere.example/','/views/'+ '1'.repeat(32),'/views/../sibling','/views/'+ '2'.repeat(32)+'?token=bad'])('invalid target fails before request', async path => {
  const c=client([]);await expect(probeScopedSibling(c.request,own,path)).rejects.toThrow(/canonical/);expect(c.get).not.toHaveBeenCalled();
});
test('transport failure is not a denied-scope pass', async ()=>{
  const c=client([]);c.get.mockRejectedValueOnce(new Error('secret'));await expect(probeScopedSibling(c.request,own,sibling)).rejects.toThrow('scope probe request failed');
});

test('untrusted view origin fails before request',async()=>{
  const c=client([]);await expect(probeScopedSibling(c.request,own.replace('.site','.site.evil.example'),sibling)).rejects.toThrow(/canonical/);expect(c.get).not.toHaveBeenCalled();
});
