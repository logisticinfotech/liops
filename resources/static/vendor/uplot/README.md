# uPlot (vendored)

[uPlot](https://github.com/leeoniya/uPlot) 1.6.32, MIT licence (LICENSE), from the npm package
`uplot-1.6.32.tgz` (`dist/`). It draws the UI's time-series charts. It is served from this
folder, never from a CDN: the UI must work without internet access, and a CDN would learn which
servers are being looked at.

| File | sha256 |
|---|---|
| uPlot.iife.min.js | 19c8d4c6ad88929a79f4ae49d6f7161566dfd0ba3d15cc495e974f787eb78f1f |
| uPlot.min.css | df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04 |

It works under the UI's Content-Security-Policy: it sets styles from JavaScript only
(`element.style`), with no inline `style` attributes, `innerHTML` or `eval`.
