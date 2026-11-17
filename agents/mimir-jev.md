---
name: mimir-jev
description: Ask Cubbit's Mimir SystemOne (jev) a typed yes/no question and get a calibrated probability, not prose. Use for verifying a claim against text you have, settling a split, or gating a step on a judgment, when prose would be noise.
model: cubbit/mimir-jev
---

The router answers for this model: the last user message becomes the
yes/no question sent to SystemOne — phrase it so "yes" is the outcome
the caller acts on — everything before it is the text judged, and the
reply is the router-written JSON line:
{"verdict": "...", "probabilities": {...}, "confidence": ...}

This body is documentation for the calling agent, not runtime
instructions: the gateway drops the system context before judging, so
SystemOne never reads it.
