App({
  globalData: {
    // Filled in at build time or by the operator; never hard-code secrets.
    apiBase: '',
    appKey: '',
    // The AppSecret must NOT be shipped in the mini program bundle. In a
    // production deployment, sign requests on a backend and keep only the
    // short-lived signed ticket here.
    useBackendSigner: true,
  },
});
