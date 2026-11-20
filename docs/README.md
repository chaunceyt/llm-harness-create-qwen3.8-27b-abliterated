# Experiment

Have an open weight LLM write a harness in Go in one-shot. 

## The prompt

I use Google search to help generate prompts after a couple of back queries this [prompt](prompt.md) was good enough.


## The LLM

The LLM used was [Blackfrost-AI/Qwen3.8-27B-ABLITERATED](https://huggingface.co/Blackfrost-AI/Qwen3.8-27B-ABLITERATED-GGUF) quant: `Q8_0`

```bash
llama-server -m /Volumes/development3/ggufs/Qwen3.8-27B/Qwen3.8-27B-ABLITERATED-Q8_0.gguf \
    --spec-type draft-mtp \
    --spec-draft-n-max 3 \
    -ngl 999 \
    --jinja \
    -c 262144 \
    -ctk q8_0 \
    -ctv q8_0 \
    --host  $(ipconfig getifaddr en1) \
    --port 8099
```

## Harness

Claude code with `ANTHROPIC_BASE_URL=` set to point to my internal LLM server. Running llama.cpp as the inference engine. 

## The compute

An Apple M3 Ultra chip with 32-core CPU, 80‑core GPU, 32-core Neural Engine with 512GB unified memory

## Runtime

Openshell 0.1.3  

## Results

Time to complete: ~9hrs

The one-shot resulted in code that compile and ran but the following error was generated.

```bash
Got the following error testing. \
  sandbox@094f02f27e8d:~/harness-qwen3.8$ ./harness --provider openai --model local-llm --endpoint http://192.168.4.24:9991 --workspace $WORKSPACE --store :memory:     "run:
  uname -s"
  harness: note: no  API key in environment; assuming a local endpoint
  panic: runtime error: invalid memory address or nil pointer dereference
  [signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x49ce5c]

  goroutine 1 [running]:
  harness/internal/agent.merge(...)
      /sandbox/harness-qwen3.8/internal/agent/agent.go:496
  harness/internal/agent.(*Agent).consume(0x23fe772ef8a8, {0xa1f728?, 0x23fe774841b0?}, {{0xffffe2faec96, 0x9}, {0x51fc58, 0xaa}, {0x23fe774ac000, 0x1, 0x1}, ...})
      /sandbox/harness-qwen3.8/internal/agent/agent.go:365 +0x32c
  harness/internal/agent.(*Agent).run(0x23fe772ef8a8, {0xa1f8e8, 0x23fe7735e680}, {0xffffe2faecfa, 0xd})
      /sandbox/harness-qwen3.8/internal/agent/agent.go:265 +0x9a4
  harness/internal/agent.(*Agent).Run(...)
      /sandbox/harness-qwen3.8/internal/agent/agent.go:195
  main.runTask({0xa1f8e8?, 0x23fe7735e680?}, 0x0?, {0xffffe2faecfa?, 0x500c69?}, {0xa1cc70, 0x23fe772c8058})
      /sandbox/harness-qwen3.8/cmd/harness/main.go:229 +0x78
  main.run({0x23fe772dc0d0, 0xb, 0xb}, {0xa1cc70, 0x23fe772c8050}, {0xa1cc70, 0x23fe772c8058}, {0xa1cc30, 0x23fe772c8048})
      /sandbox/harness-qwen3.8/cmd/harness/main.go:224 +0x120c
  main.main()
      /sandbox/harness-qwen3.8/cmd/harness/main.go:29 +0x7c
```

Issue identifed

> The crash is a real nil-pointer hole in merge: consume's first EventUsage arrives with usage still nil, and merge(nil, b) dereferences the nil first argument (agent.go:496).
  The scripted test providers never emitted usage events, so nothing exercised that path — your live endpoint did. Fixing merge to be nil-safe, adding regression tests (direct
  merge units plus a scripted usage-carrying turn), and fixing the cosmetic no  API key message while I'm in there.


After that fix, we were able to start building things.