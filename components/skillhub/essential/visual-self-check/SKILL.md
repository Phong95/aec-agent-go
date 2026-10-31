---
name: visual-self-check
description: Verify visual work by rendering it and looking at it, then fix and look again, before calling it done. Use after building or changing anything people see — a web page, a component, a chart, an animation, an exported image — and when the user asks you to design something or to check how it looks. Covers automated checks first, choosing what to capture, reading a contact sheet then full-size captures, a fix-and-recapture loop with a stop rule, and an honest report of what screenshots cannot prove.
metadata:
  requires-tools: screenshot
---

# Visual Self-Check

Code that runs is not a picture that looks right. For anything a person will see, render it, capture it, look at the captures yourself, and only then say it works. You are the reviewer: read the images, don't guess from the code.

The loop:

```
automated checks → capture → look (sheet, then full size) → fix the cause → recapture what changed
        ↑_______________________________________________________________________|
             stop: a clean pass, 3 rounds, or a call only a human can make
```

## 1. Run the automated checks first

Cheap, objective checks catch mechanical failures before you spend attention on pixels: the project's lint, type check, tests or build. Fix errors here first. Read warnings; say which ones you left and why. A passing check is the entry ticket to the visual pass, not a substitute for it.

## 2. Decide what to capture

Capture the moments where something can go wrong, not one random view:

| Output | Capture |
|---|---|
| Page or component | each size that matters (e.g. `viewports: ["375x812", "1440x900"]`), `dark: true` too when the design has a dark theme, and the empty, loading, error and full states |
| One element | `selector` — the card, the chart, the header — instead of the whole page |
| Long page | `full_page: true` |
| Animation | `at` the middle of each scene, the last frame, and both sides of every cut, with `seek` moving the timeline to `%t` |
| Running dev server | `service` with the page path |
| Anything with text | the longest real string in each text slot, and every language on screen |

Use `wait_ms` when the page needs time after loading (a chart that animates in, data that arrives later). The tool already waits for fonts and images.

## 3. Look: contact sheet first, then full size

- Up to four captures come back as separate images. More come back as one labelled contact sheet: read it for the whole — consistency between sizes, one view unlike the others, an element that jumps across a cut.
- Then capture the views the sheet flagged (and the main one) on their own, at full size. Small text, accents, 1 px lines, icon edges and colours are invisible in a thumbnail.
- Use `selector` to zoom into a region that is still unclear.
- Read the **page errors** and **failed requests** the result lists: a console error or a 404 image is a bug even when the page looks fine.

Go through the checklist for every capture you open, and write each problem down with where it is ("375x812: the third card overflows the screen").

### Checklist

- **Layout:** nothing overflows its box or the screen; no horizontal scroll on a phone; alignment to the design's grid; consistent spacing between siblings.
- **Text:** no unintended wrapping (a lone word on its own line), truncation or overlap; accents not clipped; the real copy, not placeholder text.
- **Hierarchy:** one element clearly dominates; the eye lands where the brief wants it.
- **Colour:** the design's colours, not near misses; text readable on its background, in both themes.
- **Assets:** the right image, not stretched, blurry or cropped through something important; no broken-image icons; transparent edges clean.
- **Motion:** an element that continues across a cut keeps its position, size and opacity; nothing appears early or freezes mid-entrance.
- **States:** empty, loading and error states look designed, not broken.

## 4. Fix the cause, then recapture

- Fix the smallest cause in the source (the CSS rule, the timing, the asset), not the symptom in one view.
- When an exported or converted asset looks wrong, suspect the conversion before the source.
- Recapture only what the fix touches, plus its neighbours across any cut, and look again. Older screenshots drop out of your context after a few captures: take a new one rather than relying on memory.

## 5. Stop rule

Stop when one of these is true:

- every checklist item passes on every capture;
- three fix-and-recapture rounds have run — report what is still open instead of looping on;
- the remaining question is taste or judgment that belongs to a person (pacing, mood, "does this feel premium").

## 6. Report honestly

Say what you checked and at which sizes, list what you fixed, show the page with `preview` so the user can look too, and say plainly what screenshots cannot prove:

- **Motion quality** between the captured moments: smoothness, easing, timing.
- **Sound.**
- **Real devices:** a browser viewport is not a phone; touch targets and system bars are approximations.
- **Fonts and media from the network:** what loaded here may not load elsewhere.
