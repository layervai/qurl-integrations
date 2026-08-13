function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

function cookieValue(setCookie, name) {
  const header = Array.isArray(setCookie) ? setCookie.join('\n') : setCookie || '';
  const match = header.match(new RegExp(`${escapeRegExp(name)}=([^;\\n]+)`));
  return match ? decodeURIComponent(match[1]) : null;
}

function clearedCookieHeader(setCookie, name) {
  const headers = Array.isArray(setCookie) ? setCookie : [setCookie || ''];
  const namePattern = new RegExp(`^${escapeRegExp(name)}=`);
  return headers.find((h) =>
    namePattern.test(h) && /Expires=Thu, 01 Jan 1970|Max-Age=0/i.test(h));
}

module.exports = {
  clearedCookieHeader,
  cookieValue,
};
