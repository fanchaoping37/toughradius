// Mini-program API helper for the ToughRADIUS Portal bridge and open API.
//
// Security note: a WeChat mini program bundle is public. Never embed the
// AppSecret or Portal shared secret here. Set useBackendSigner=true and have
// a backend endpoint return a short-lived signed request (or a Portal
// auth_ticket) that this client simply forwards.

const app = getApp();

function baseUrl() {
  return (app.globalData.apiBase || '').replace(/\/$/, '');
}

function request(path, method, data, headers) {
  return new Promise((resolve, reject) => {
    wx.request({
      url: baseUrl() + path,
      method: method || 'GET',
      data: data || {},
      header: Object.assign({ 'Content-Type': 'application/json' }, headers || {}),
      success(res) {
        if (res.statusCode >= 200 && res.statusCode < 300) {
          resolve(res.data);
        } else {
          reject(new Error('HTTP ' + res.statusCode));
        }
      },
      fail(err) {
        reject(err);
      },
    });
  });
}

// Fetch a signed request from the operator backend (backend holds the secret).
function fetchSignedPayload(action, params) {
  if (!app.globalData.useBackendSigner) {
    // Local development only: the backend signer URL must be configured.
    return Promise.reject(new Error('Backend signer is required'));
  }
  return request('/api/v1/open/sign', 'POST', { action, params });
}

// Portal flow: challenge -> auth_ticket -> authenticate.
async function portalChallenge(username, userIp, mac) {
  return request('/api/v1/portal/challenge', 'POST', {
    username,
    user_ip: userIp,
    mac: mac || '',
  });
}

async function portalAuth(username, password, userIp, mac, ticket, nasAddr) {
  return request('/api/v1/portal/auth', 'POST', {
    username,
    password,
    user_ip: userIp,
    mac: mac || '',
    auth_mode: 'chap',
    auth_ticket: ticket,
    nas_addr: nasAddr || '',
  });
}

async function portalLogout(userIp, nasAddr) {
  return request('/api/v1/portal/logout', 'POST', {
    user_ip: userIp,
    nas_addr: nasAddr || '',
  });
}

// Open API call with backend-provided HMAC headers.
async function openApi(path, method, body) {
  const signed = await fetchSignedPayload(path, body || {});
  return request(path, method || 'GET', body || {}, signed.headers || {});
}

module.exports = {
  request,
  portalChallenge,
  portalAuth,
  portalLogout,
  openApi,
};
