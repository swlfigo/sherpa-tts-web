# Third-party notices

The source code of this project is under the Apache License 2.0 (see
`LICENSE`). Binary releases and container images also contain the
following software, under its own licenses.

| Component | License | Where |
|---|---|---|
| [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) and its Go bindings | Apache-2.0 | `lib/libsherpa-onnx-c-api.so`, linked into the program |
| [ONNX Runtime](https://github.com/microsoft/onnxruntime) | MIT | `lib/libonnxruntime.so` |
| [eSpeak NG](https://github.com/espeak-ng/espeak-ng) | GPL-3.0-or-later | built into `libsherpa-onnx-c-api.so` by sherpa-onnx, for the pronunciation of words missing from the lexicons |

Because the shared library includes eSpeak NG, a binary release or
container image as a whole is distributed under the terms of the GNU GPL
version 3. Its corresponding source is this repository together with the
sources of the components above, at the versions pinned in `go.mod`.

## The speech model

The model is not part of this repository, the releases or the images: the
program downloads it on first start from the Hugging Face repository
[csukuangfj/kokoro-multi-lang-v1_0](https://huggingface.co/csukuangfj/kokoro-multi-lang-v1_0),
at the revision pinned in `internal/model/manifest.json`. It is
[Kokoro-82M](https://huggingface.co/hexgrad/Kokoro-82M) (Apache-2.0)
converted for sherpa-onnx, with lexicons, dictionaries and `espeak-ng-data`;
see that repository for the license of each of its files.
