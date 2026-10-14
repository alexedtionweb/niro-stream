# Voice activity and turn detection

`VAD` and `TurnDetector` are core interfaces. None of the implementations are imported by the core module. `go get` only the one you call.

| Module | Install | What it does |
|--------|---------|----------------|
| `provider/energy` | `go get github.com/alexedtionweb/niro-stream/provider/energy` | Local VAD. RMS of PCM16 mono. No model. |
| `provider/turn` | `go get github.com/alexedtionweb/niro-stream/provider/turn` | Local turn score from how long the speaker has been quiet. |
| `provider/audiomodel` | `go get github.com/alexedtionweb/niro-stream/provider/audiomodel` | HTTP client for a VAD model and a turn-detection model. |

A VAD answers "is this chunk speech?". A turn detector answers "has the speaker finished?". Use both when a quiet gap in the middle of a sentence should not commit the turn: wait until the turn score says the turn is done, then commit.

## Local VAD

```go
vad := energy.New(energy.Config{
    Threshold: 500,                  // RMS of int16 samples
    Silence:   400 * time.Millisecond,
    Rate:      16000,
})
marks, err := vad.Push(ctx, pcm16le)
// marks[i].Kind is niro.SpeechStart or niro.SpeechEnd
```

`Push` takes one PCM16 little-endian mono chunk and scores that chunk. Call `Reset` when the stream changes.

## Local turn detector

```go
score, err := turn.Silence{After: 400 * time.Millisecond}.Predict(ctx, niro.TurnInput{
    Silence: quietFor,
})
if score.End >= 1 {
    // commit the turn
}
```

`End` is `silence / After`, and `1` once silence reaches `After`.

## Model servers

`provider/audiomodel` does not download or run a network. It POSTs to a server you already run. Any model that speaks the JSON below works: Silero behind a small HTTP wrapper, a smart-turn classifier, or your own.

VAD, one request per chunk:

```json
POST /vad
{"audio":"<standard base64 of PCM16>","sample_rate":16000}

{"speech":0.82}
```

`speech` is the probability the chunk contains speech. `Speech.On` (default 0.5) opens a run. `Speech.Off` (default 0.35) is the level below which silence starts. `Speech.Silence` (default 400ms) must then elapse before `SpeechEnd`. A failed request leaves the run unchanged.

Turn model:

```json
POST /turn
{"text":"what you heard so far","silence_ms":240}

{"end":0.91}
```

`end` is the probability the turn is finished. You pick the cutoff.

```go
vad := &audiomodel.Speech{URL: "http://127.0.0.1:9000/vad", Rate: 16000}
marks, err := vad.Push(ctx, pcm16le)

score, err := audiomodel.Endpoint{URL: "http://127.0.0.1:9000/turn"}.Predict(ctx, niro.TurnInput{
    Text:    transcript,
    Silence: quietFor,
})
```

## Realtime sessions

These detectors stay on the client. A duplex realtime session leaves server `turn_detection` unset and commits when your turn detector says the turn is finished. `server_vad` mode skips them and lets the realtime server decide.
