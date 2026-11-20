<p align="center"><img src="assets/cardo.svg" width="110" alt=""></p>

<h1 align="center">cardo</h1>

<p align="center">One model per GPU. Swap which engine holds the slot, draining first and rolling back if the new one does not answer.</p>

## Install

```bash
go install github.com/JoyanLabs/cardo@latest
```

## Use

```bash
cardo list                 # registered engines and which one is up
cardo status               # live state, memory in use
cardo up vllm-qwen         # drain, stop, start, verify
cardo down                 # stop the active engine, draining first
cardo watch                # what occupies the slot right now
```

Engines are declared in small config files, one per engine, so adding one is a file and not code.

## Status

Early. The CLI is being built; see the design notes in the repository.

## License

MIT
