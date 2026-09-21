# Assets

`logo.svg` is the source; the PNGs are rendered from it.

## The mark

A speech bubble holding a diff — the two things kibitz does, in one silhouette.
The bars use GitHub's own colours for additions and deletions, so the meaning is
already familiar wherever the mark appears.

It is drawn to survive being shrunk: GitHub renders a bot's avatar at around
40px next to a comment, and at 20px in some lists. Nothing in the mark depends
on detail that disappears at those sizes.

| Colour | Use |
| --- | --- |
| `#20304F` | Background |
| `#FFFFFF` | Bubble |
| `#3FB950` | Additions |
| `#F85149` | Deletions |

## Uploading it to the GitHub App

Use `kibitz-app-logo-1024.png` on the App's settings page (Display information
→ Logo). GitHub wants a square image of at least 200x200; the 512 and 200 are
there for anywhere that needs something smaller.

The background is full bleed on purpose: GitHub applies its own corner
rounding, and a coloured square keeps the mark from picking up whatever sits
behind it.

## Re-rendering

```bash
npm install sharp
node -e "
const sharp = require('sharp');
const svg = require('fs').readFileSync('assets/logo.svg');
for (const size of [1024, 512, 200]) {
  sharp(svg, { density: 400 }).resize(size, size)
    .png({ compressionLevel: 9 })
    .toFile('assets/kibitz-app-logo-' + size + '.png');
}
"
```
