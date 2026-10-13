'use strict';
window.Scalar.createApiReference('#api-reference', {
  url: '/openapi.yaml',
  theme: 'purple',
  darkMode: true,
  withDefaultFonts: false,
  persistAuth: false,
  telemetry: false,
  agent: {disabled: true},
  hideClientButton: true,
  showDeveloperTools: 'never',
  proxyUrl: '',
  servers: [{url: window.location.origin}],
});
