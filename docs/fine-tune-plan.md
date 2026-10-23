# Plan: fine-tuning Qwen on its own accepted work

**Goal.** Teach qwen3-coder:30b the habits it keeps getting reviewed for
(tests that can fail, one test per case, the code's own names, what the task
named) by training it on the Away attempts you accepted, instead of only
telling it in QWEN.md. Ship it only if the benchmark and held-out Away tasks
say it's better.

**When.** About 100 accepted Qwen-only examples (28 on 10-22). Refills
(`refill.go`) make that a few weeks of normal use. A cheap dry run can happen
earlier, with ~40, to shake out the pipeline (step 2).

## What we have (10-22)

| | |
|---|---|
| Accepted attempts | 47: 28 Qwen-only, 19 that Claude finished |
| Typical Qwen-only attempt | 1 round, ~10 tool calls, ~24,000 characters of transcript (~6-8k tokens); longest ~135,000 |
| Where | `away-examples/<project>/*.json` on the PC (tool results clipped), with the full transcripts in `logs/away/<id>.roundN.qwen.json` |

## The data

- **In:** accepted, author `qwen` (Qwen alone passed review), including
  accepted drills. For an attempt that took two Qwen rounds, the last round,
  with the reviewer's notes as the user message that starts it: that's the
  behaviour to learn, answering feedback.
- **Out:** anything Claude finished (`qwen+claude`), discarded or failed
  attempts, and the 4k-context and broken-answer rounds. Later, Claude's
  finished versions could become "here's how it should have gone" examples,
  but not in the first run.
- **Format:** each attempt becomes one chat: Loom's system prompt and tool
  definitions as the model actually saw them, the task, then the assistant
  turns with their tool calls in qwen3-coder's own tool-call format and the
  tool results. Loss on the assistant turns only. Full transcripts from the
  logs, not the clipped ones in the examples.
- **Held out:** 15% of the examples, picked by task, plus the 11 benchmark
  tasks (`loom/tools/bench`), never trained on.
- **Privacy:** the training data is Parliament-Dev and Skirmish code. It goes
  to whichever GPU does the training. A rented one (option A) means
  uploading it; option B keeps it on the PC.

## Two ways to train

| | A. Rent a GPU, train the 30B model | B. Train a smaller model on the PC |
|---|---|---|
| Model | qwen3-coder 30B-A3B (what Away runs now) | an 8B-class coder (e.g. Qwen3-8B) |
| Method | QLoRA with Unsloth; about 17.5 GB of VRAM at 2k context, more at 8-16k, so an 80 GB card | QLoRA with Unsloth in the 5070's 12 GB, short contexts |
| Cost | A100 80 GB about $1.19-1.49/hour on RunPod; a run on ~100 examples should be under an hour, so a few dollars | Free, but slow, and the PC is busy while it trains |
| Risk | Uploading code; Ollama's adapter import isn't confirmed for Qwen3, so merge the adapter and convert to GGUF instead | Starts from a weaker model: it has to beat the 30B on the benchmark to be worth it |

Recommendation: **A**, because it improves the model Away already uses, and
the cost is a few dollars a run.

## Steps

1. **Exporter** (Homebase, small): `homebase -export-training <dir>` writes
   train/held-out JSONL from the PC's examples and logs, using the rules
   above. It needs Loom to print the exact system prompt and tool definitions
   for a project (a small script in `loom/tools/qwen`).
2. **Dry run** (~40 examples, one rented hour): train, merge, convert to GGUF
   (Q4_K_M, like the current model), `ollama create qwen3-coder-away`, and run
   the benchmark against both models. The point is the pipeline, not a better
   model yet.
3. **Real run** at ~100 Qwen-only examples, same steps.
4. **Decide:** point Away at the new model (`away.model` in homebase.json)
   only if it beats the current one on the benchmark *and* on the held-out
   Away tasks (first-try passes, rounds, how often Claude has to finish).
   Keep the old one for switching back.
5. **Repeat** every ~100 new examples.

## Open questions

- Unsloth's figure is for Qwen3-30B-A3B; check its notebook for the Coder
  variant and for long contexts before renting anything.
- qwen3-coder's tool-call format in Ollama (XML-style function calls) has to
  match what training uses, or the tuned model will garble its calls (as on
  10-20, "expected element type <function>").
- Keep full transcripts: confirm nothing prunes `logs/away/`.

Sources: [Qwen + Unsloth guide](https://qwen.readthedocs.io/en/latest/training/unsloth.html),
[RunPod pricing](https://www.runpod.io/gpu-cloud/pricing),
[JarvisLabs A100 prices](https://jarvislabs.ai/blog/a100-price),
[Ollama import (adapters)](https://docs.ollama.com/import).
