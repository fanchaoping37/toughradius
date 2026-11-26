const api = require('../../utils/api');

Page({
  data: {
    username: '',
    password: '',
    userIp: '',
    mac: '',
    nasAddr: '',
    loading: false,
    message: '',
  },

  onLoad(options) {
    // Parameters are supplied by the QR code / Portal URL that launched the
    // mini program: ?ip=<client-ip>&mac=<mac>&nas=<nas-addr>
    this.setData({
      userIp: options.ip || '',
      mac: options.mac || '',
      nasAddr: options.nas || '',
    });
  },

  onUsername(e) { this.setData({ username: e.detail.value }); },
  onPassword(e) { this.setData({ password: e.detail.value }); },

  async onLogin() {
    const { username, password, userIp, mac, nasAddr } = this.data;
    if (!username || !password) {
      this.setData({ message: '请输入用户名和密码' });
      return;
    }
    if (!userIp) {
      this.setData({ message: '缺少客户端 IP，请从 Portal 二维码进入' });
      return;
    }
    this.setData({ loading: true, message: '' });
    try {
      // Stage 1: obtain a one-time auth_ticket bound to user_ip + MAC.
      const challenge = await api.portalChallenge(username, userIp, mac);
      if (challenge.code !== 0 || !challenge.data) {
        throw new Error(challenge.message || '挑战失败');
      }
      // Stage 2: authenticate with the ticket (single-use).
      const auth = await api.portalAuth(
        username, password, userIp, mac, challenge.data.auth_ticket, nasAddr
      );
      if (auth.code !== 0 || !auth.data || !auth.data.authenticated) {
        throw new Error(auth.message || '认证失败');
      }
      this.setData({ message: '认证成功，可以访问网络了' });
    } catch (err) {
      this.setData({ message: err.message || '网络错误，请重试' });
    } finally {
      this.setData({ loading: false });
    }
  },

  async onLogout() {
    const { userIp, nasAddr } = this.data;
    if (!userIp) return;
    this.setData({ loading: true, message: '' });
    try {
      const res = await api.portalLogout(userIp, nasAddr);
      this.setData({ message: res.code === 0 ? '已下线' : (res.message || '下线失败') });
    } catch (err) {
      this.setData({ message: err.message || '网络错误' });
    } finally {
      this.setData({ loading: false });
    }
  },
});
