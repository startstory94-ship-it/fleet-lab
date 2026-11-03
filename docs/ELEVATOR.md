# 엘리베이터 프로토콜 (6단계)

목적층이 다른 로봇 여러 대를 엘리베이터 한 대에 함께 태우는 규약이다. 6a(서버, `internal/elevator`·`internal/task/ride.go`·`internal/sim`)가
이 문서대로 동작하고, 6b(ROS mock 로봇, 어댑터, mock 엘리베이터, 대시보드, e2e)는 이 문서대로 만든다. 코드와 이 문서가 다르면 버그다.

## 1. 핵심 규칙

- **엘리베이터 안에서는 로봇 와이파이(`fleet/*`)가 끊긴다. 엘리베이터 채널(`elevator/<id>/*`)은 살아 있다.**
  그래서 탑승 계획은 **타기 전에** 로봇에게 보내고 `accepted`를 받는다. 탄 뒤에는 로봇이 그 계획과 엘리베이터 채널만으로 움직인다.
- **탑승 순서 = 하차 순서의 반대.** 먼저 내리는 로봇이 마지막에 타서 문 쪽(칸 0)에 선다. 그래서 어느 층에서든 내릴 로봇이 남는 로봇보다 앞에 있다.
- **문을 지나가는 것은 한 번에 한 대.** 서버(지휘자)가 `turn`으로 한 로봇에게만 문을 준다. 다음 `turn`은 세 조건이 모두 맞을 때만 나간다:
  ① 열린 turn이 없다(앞 로봇이 `zone` `clear`/`failed`로 답했거나 시간 초과로 닫힘) ② 엘리베이터가 최근(5초 안) 그 층·그 운행으로 문 열림을 알렸다
  ③ **앞 turn이 닫힌 뒤에 받은 엘리베이터 상태가 `door_zone: 0`(문 구역 비어 있음)이다.** 시간 초과는 절대 다음 로봇을 움직이게 하지 않는다:
  같은 로봇의 실패한 시도로 센다(재시도 → 다음 층으로 넘김/사람 호출). 로봇은 자기 `turn`이 없으면 절대 문 구역에 들어가지 않는다.
- **로봇이 문을 지나는 동안 문은 열려 있다(hold).** 지휘자가 `hold`를 계속 보내고, 마지막 로봇이 지나간 뒤 `goto`(다음 층) 또는 `release`로 놓는다.
- 같은 방향만 함께 탄다. 칸이 모자라거나 방향이 다른 로봇은 다음 운행을 기다린다. 목적층이 지금 층이면 엘리베이터를 타지 않는다(그냥 `goto`).

역할:

| 누구 | 하는 일 |
|---|---|
| fleetd (지휘자, `elevator.Car`) | 로비에서 기다리는 로봇을 모아 계획, 엘리베이터 호출·이동·문 유지, `turn`으로 한 대씩 문을 줌, 실패 처리, 임대(lease) |
| 엘리베이터 (6b의 `mock_elevator`, 실제로는 벤더 API 게이트웨이) | 명령대로 이동·문 열고 닫기·유지, 상태 발행. 로봇이 문 구역에 있으면 문을 닫지 않음 |
| 로봇 어댑터 (6b의 `fleet_adapter.py`) | `ride` 명령 수락, 엘리베이터 채널 구독, 자기 `turn`에만 움직이고 `zone`으로 결과 알림, 끊김 예고(`silence_s`), 끊긴 동안 이벤트 쌓아 두기, 나온 뒤 `done` |

## 2. 토픽

| 토픽 | 방향 | QoS | retained | 내용 |
|---|---|---|---|---|
| `fleet/<robot>/cmd` | 서버 → 로봇 | 1 | 아니오 | `type: "ride"` 명령 (그 밖의 명령은 README) |
| `fleet/<robot>/cmd_ack` | 로봇 → 서버 | 1 | 아니오 | `ride`의 `accepted` / `done`(+`floor`) / `failed` / `cancelled` |
| `fleet/<robot>/state` | 로봇 → 서버 | 1 | 아니오 | 탑승 직전 `silence_s`(끊김 예고), 모드 `WAITING_ELEVATOR`·`BOARDING`·`RIDING`·`EXITING` |
| `fleet/<robot>/event` | 로봇 → 서버 | 1 | 아니오 | 로봇이 겪은 일(seq + 로봇 시각). 끊긴 동안 쌓았다가 다시 연결되면 보냄 |
| `elevator/<id>/cmd` | 서버 → 엘리베이터 | 1 | 아니오 | `call` / `goto` / `hold` / `release` |
| `elevator/<id>/state` | 엘리베이터 → 모두 | 1 | **예** | 층, 방향, 문, 이동 중, 유지, 예약(trip), 탑승 수 |
| `elevator/<id>/trip` | 서버 → 로봇·서버 | 1 | **예** | 운행 전체(단계, 로봇별 상태). 재시작한 서버가 이어받는 데 씀. 끝나면 마지막 단계를 보낸 뒤 **빈 retained 메시지로 지운다**. 10초마다, 그리고 MQTT 재연결 때 다시 보낸다 |
| `elevator/<id>/turn` | 서버 → 로봇 | 1 | 아니오 | "이 로봇, 지금 문을 지나가라". 답이 올 때까지 5초마다 **같은 `n`으로** 다시 보낸다 |
| `elevator/<id>/zone` | 로봇 → 서버·엘리베이터 | 1 | 아니오 | 문 구역에 들어감(`enter`), 벗어남(`clear`), 못 지나감(`failed`) |

fleetd 구독: `elevator/+/state`, `elevator/+/zone`, `elevator/+/trip`, `fleet/+/event` (기존 `fleet/+/pose|state|cmd_ack`, `fleet/adapter/+/status`에 더해).
로봇(어댑터) 구독: `ride`를 수락한 순간부터 `elevator/<id>/turn`, `elevator/<id>/trip`, `elevator/<id>/state`. 차에서 내려 `done`을 보낸 뒤 해지해도 된다.

시각: `stamp`는 보낸 쪽 시계의 유닉스 초(소수). 서버의 판단(시간 제한)은 모두 **서버가 받은 시각** 기준이고 `stamp`는 기록용이다.

## 3. 메시지

### 3.1 `ride` 명령 (`fleet/<robot>/cmd`)

```json
{"cmd_id":"t-3.0.1.k3x9ab.R2","type":"ride","x":10,"y":0,"yaw":0,"expires_at":1793696410.25,
 "ride":{"trip":"E1-k3x9ab-1","elevator":"E1","origin":1,"direction":"up","exit_floor":3,
         "board":2,"slot":0,"boarding":["robot_2","robot_3","robot_1"],"stops":[3,5,7],
         "silence_s":58,"turn_timeout_s":15,
         "lobby":{"x":10,"y":0},"exit_lobby":{"x":10,"y":0}}}
```

| 필드 | 뜻 |
|---|---|
| `cmd_id` | `<작업>.<구간>.<배정 횟수>.<서버 실행 ID>.R<번호>`. 운행마다 새 번호. 다른 명령처럼 같은 ID는 한 번만 실행하고, 중복이면 마지막 응답을 다시 보낸다 |
| `x`, `y` | `ride.lobby`와 같다(참고용) |
| `expires_at` | 받은 시각이 이보다 늦으면 실행하지 않고 `failed: expired` |
| `ride.trip` | 운행 ID. `turn`·`zone`·`event`에 그대로 쓴다 |
| `ride.elevator` | 엘리베이터 ID. 구독할 `elevator/<id>/*` |
| `ride.origin` / `direction` | 타는 층, `up`/`down` |
| `ride.exit_floor` | 계획상 내리는 층 |
| `ride.board` | 탑승 순서(0이 먼저). 정보용: 실제 순서는 `turn`이 정한다 |
| `ride.slot` | 칸 번호(0이 문 쪽). 탈 때 이 칸까지 들어간다 |
| `ride.boarding` | 모든 탑승 로봇(탑승 순서) |
| `ride.stops` | 정차 층(이동 순서) |
| `ride.silence_s` | 탑승할 때 `state`로 예고할 끊김 시간(초). 서버가 계산(아래 5장) |
| `ride.turn_timeout_s` | 서버가 turn 하나를 기다리는 최대 시간 |
| `ride.lobby` / `exit_lobby` | 타는 층에서 기다리는 곳 / 내리는 층에서 나와 설 곳 |

로봇(어댑터)은:
- 로비(`lobby`)에 있고 ERROR가 아니면 `accepted`. 아니면 `failed`(메시지에 이유).
- 수락 뒤 `state`의 `task_id`는 이 `cmd_id`, 모드는 `WAITING_ELEVATOR`.
- **먼저 `cmd_id` 중복을 확인한다.** 이미 처리한 `cmd_id`면 계획(`ride`)이 없어도 마지막 응답을 다시 보낸다. 서버가 운행이 끝난 뒤 `done`을 다시 물을 때는
  `ride` 없이 같은 `cmd_id`를 보낸다. 처음 보는 `cmd_id`인데 `ride`가 없으면 `failed`.
- 들고 있는 `ride`가 아직 탑승 전(`WAITING_ELEVATOR`)이면 새 `ride`가 그것을 대신한다: 옛 것은 `cancelled`로 끝낸다.
- `ride`를 들고 있는 동안 `goto`·`dock`은 `failed`로 거절한다(서버는 그런 명령을 보내지 않는다).

### 3.2 `ride` 응답 (`fleet/<robot>/cmd_ack`)

| `status` | 언제 | 서버 |
|---|---|---|
| `accepted` | 계획을 받음(로비에서) | 모든 탑승 로봇이 수락해야 탑승 시작 |
| `done` | **엘리베이터에서 나와 와이파이가 돌아온 뒤.** `floor`에 지금 층 | `floor`가 작업 지점 층이면 그 지점으로 `goto`, 아니면 그 층에서 경로를 다시 짠다(`replanned`) |
| `failed` | 수락 못 함, 만료, 탈 수 없게 됨(탑승 전) | 시도 1번 쓰고 작업을 다시 대기열로 |
| `cancelled` | 탑승 전 `cancel`을 받아 계획을 버림 | 서버가 요청한 것이므로 무시 |

```json
{"cmd_id":"t-3.0.1.k3x9ab.R2","status":"done","message":"","floor":3}
```

`floor`가 없으면 서버는 마지막 `pose`의 층을 쓴다. 6b는 항상 넣는다.

### 3.3 `cancel` (탑승 전 계획 버리기)

서버가 운행을 중단하거나(abort) 로봇을 빼면 `cancel`을 보낸다.

```json
{"cmd_id":"t-3.0.1.k3x9ab.R2.x1793696412345","type":"cancel","target":"t-3.0.1.k3x9ab.R2","expires_at":1793696422.3}
```

`target`이 멈출 명령이다(`cmd_id`는 겹치지 않게 만든 것일 뿐, 거기서 대상을 잘라 내지 않는다: 서버 실행 ID가 `x`로 시작할 수도 있다).
- **`target`이 지금 들고 있는 `ride`이고 아직 탑승 전**이면: 계획을 버리고 `ride`를 `cancelled`로 끝내고, `cancel`은 `accepted`. 로봇은 로비에 그대로 있다.
- 대상이 다른 명령(이미 버린 옛 `ride` 등)이면 아무것도 하지 않고 `cancel`은 `failed`. 늦게 도착한 옛 취소가 새 계획을 지우지 않게 하기 위해서다.
- 문을 지나기 시작했거나 안에 있으면 받을 수 없다(와이파이 없음). 받더라도 무시하고 `failed`. 엘리베이터는 그 로봇을 계획대로 내려 준다.

(`goto`에 대한 `cancel`은 지금처럼 하고 있는 것을 멈춘다. 대상 확인 규칙은 `ride`에만 적용한다.)

### 3.4 로봇 상태 (`fleet/<robot>/state`)

| 시점 | `mode` | `task_id` | `silence_s` |
|---|---|---|---|
| `ride` 수락 후 로비에서 | `WAITING_ELEVATOR` | ride `cmd_id` | 없음 |
| 자기 탑승 `turn`을 받은 직후, **움직이기 전에 한 번** | `BOARDING` | ride `cmd_id` | `ride.silence_s` |
| (차 안: 보내지 못함) | `RIDING` / `EXITING` | | |
| 내려서 와이파이가 돌아온 첫 상태 | `IDLE` | `""` (로봇 쪽에서 ride는 끝났다. `done`을 곧바로 보낸다) | **없음(0)** — 이것이 서버의 IN_ELEVATOR를 끝낸다 |

서버는 운행이 끝난(또는 모르는) ride `cmd_id`를 로봇이 6초(`2 × AckTimeout`) 넘게 보고하면 옛 계획을 들고 있는 것으로 보고 `cancel` 후 다시 대기시킨다.
그래서 내린 뒤에는 `task_id`를 비우고 `done`을 보낸다. `done`이 유실되면 서버가 같은 `cmd_id`로 다시 물어보고 어댑터는 `done`을 다시 보낸다.

- `silence_s`를 받은 서버는 그 로봇을 `IN_ELEVATOR`로 보고 그 시간 동안 OFFLINE으로 바꾸지 않는다. 시간을 넘기면 `elevator_overdue` 경보 뒤 STALE, 그 뒤 20초면 OFFLINE(2단계 규칙).
- 탑승 `state`가 유실될 수 있으므로 QoS 1로 보내고, 와이파이가 남아 있는 동안 1초마다 같은 값으로 다시 보낸다(`silence_s` 반복은 기한을 받은 시각부터 다시 센다).

### 3.5 로봇 이벤트 (`fleet/<robot>/event`)

```json
{"boot":"a1b2c3","seq":17,"stamp":1793696431.6,"event":"exited","trip":"E1-k3x9ab-1",
 "cmd_id":"t-3.0.1.k3x9ab.R2","floor":3,"detail":""}
```

- `seq`는 로봇(어댑터 실행)마다 1씩 늘어난다. `boot`가 바뀌면 다시 시작한다.
- 끊긴 동안 생긴 이벤트는 쌓아 두었다가 와이파이가 돌아오면 보낸다(순서 상관없음).
- 서버는 seq 순서로 기록한다: 다음 번호면 바로, 빈 번호가 있으면 최대 1초(`EventReorder`) 기다린 뒤 건너뛰고 기록(로그 경고). 중복·옛 번호는 버린다.
  기록은 `type: "elevator"`, `kind: "robot_event"`, `detail: "#17 exited at 09:00:31.600 (robot clock), floor 3, trip E1-k3x9ab-1"`.
- 이벤트 이름(6b가 보낼 것): `ride_accepted`, `ride_cancelled`, `boarding`(선택), `boarded`, `board_failed`, `exit_failed`, `exited`, `rescued`(사람이 꺼냄).
  (6b 어댑터: `ride_accepted`·`ride_cancelled`·`boarding`은 와이파이가 있을 때 바로, 나머지는 쌓았다가. `rescued`는 사람이 꺼냈을 때.) 서버는 이름을 해석하지 않고 기록만 한다.
  (판단은 `zone`과 `cmd_ack`로 한다. 이벤트는 차 안에서 일어난 일의 기록이다.)

### 3.6 엘리베이터 명령 (`elevator/<id>/cmd`)

```json
{"cmd_id":"E1-k3x9ab-1.c3.k3x9ab","cmd":"goto","trip":"E1-k3x9ab-1","floor":5,"hold_s":15}
```

| `cmd` | 엘리베이터가 할 일 |
|---|---|
| `call` | `trip`으로 예약. `floor`로 가서 문을 열고 `hold_s` 동안 열어 둔다. 이미 그 층이면 바로 연다 |
| `goto` | 문을 닫고(문 구역에 로봇이 있으면 기다림) `floor`로 가서 열고 `hold_s` 동안 열어 둔다 |
| `hold` | `trip`이 지금 예약이고 `floor`에 서 있고 문이 열려 있거나 열리는 중이면 지금부터 `hold_s` 동안 더 열어 둔다. **닫힌(닫히는) 문은 다시 열지 않는다** — 다시 여는 것은 같은 층으로의 `goto`다 |
| `release` | `trip`이 지금 예약이면 유지를 끝내고 예약을 푼다. 문은 평소대로 닫힌다 |

- 같은 `cmd_id`는 한 번만 적용한다. 서버는 `call`/`goto`가 상태에 보일 때까지 5초마다 **같은 `cmd_id`로** 다시 보낸다.
- `hold_s`가 지나면 유지가 끝난다. 서버가 죽어도 문이 영원히 열려 있지 않게 하기 위해서다(서버는 문 구역 사용 중 5초마다 `hold`).
- 문 구역에 무엇이든 있으면 문을 닫지 않는다(엘리베이터 자체 안전 기능).
- **문 구역 센서(`door_zone`):** 실제 엘리베이터는 문 센서(광 커튼 등)로 센다. 6b mock은 turn과 상관없이 `elevator/<id>/zone`만 보고 센다:
  어떤 로봇이 `enter`를 보내면 +1, 그 로봇의 `clear`나 `failed`가 오면 -1(같은 로봇이 두 번 세지 않게 로봇 이름 집합으로). 바뀔 때 바로 상태를 보낸다.

### 3.7 엘리베이터 상태 (`elevator/<id>/state`, retained)

```json
{"boot":"car-7","seq":812,"stamp":1793696420.1,"floor":5,"dir":"idle","door":"open","moving":false,
 "hold":true,"trip":"E1-k3x9ab-1","load":2,"cmd_id":"E1-k3x9ab-1.c3.k3x9ab","door_zone":0}
```

| 필드 | 뜻 |
|---|---|
| `boot`, `seq` | 엘리베이터 제어기 실행 ID, 1씩 증가. 서버는 같은 `boot`의 옛 `seq`를 버린다 |
| `floor` | 지금 층(이동 중이면 마지막으로 지난 층) |
| `dir` | `up` / `down` / `idle` |
| `door` | `open` / `closed` / `opening` / `closing` |
| `moving` | 층 사이 이동 중 |
| `hold` | 요청으로 문을 열어 두는 중 |
| `trip` | 예약한 운행 ID, 없으면 `""` |
| `load` | 안에 있다고 감지한 로봇 수(정보용) |
| `door_zone` | 지금 문 구역에 있는 로봇(물체) 수. **0이어야 지휘자가 다음 turn을 준다** |
| `cmd_id` | 마지막으로 적용한 명령 |

- 바뀔 때마다 + 1초마다 보낸다. **서버는 5초(`Silence`) 동안 상태가 없으면 응답 없음으로 본다.**
- 서버가 "문 열림"으로 인정하는 조건: `trip`이 그 운행, `floor`가 그 층, `door == "open"`, `moving == false`, 받은 지 5초 안.
  문이 닫혀 있으면(유지 시간이 지났거나 재시작 뒤) turn을 주지 않고 같은 층으로 `goto`를 5초마다 보내 다시 연다. 탑승·하차 중 상태가 5초 없으면 `overdue`("car not answering") 경보, 그동안 아무도 움직이지 않는다.

### 3.8 운행 (`elevator/<id>/trip`, retained)

서버가 단계가 바뀔 때마다, 그리고 10초마다·MQTT 재연결 때 운행 전체를 보낸다. 끝나면(`done`/`aborted`) 마지막으로 한 번 보낸 뒤 빈 retained 메시지로 지운다.
로봇은 단계(`stage`)를 보고 `aborted`면 탑승 전 계획을 버려도 된다(서버는 `cancel`도 보낸다).
**로봇은 이 메시지의 `turn`으로 움직이면 안 된다.** retained라 오래된 것일 수 있다. 차례는 `elevator/<id>/turn`으로만 받는다(서버가 5초마다 다시 보낸다).
`boot`는 지금 이 운행을 맡은 fleetd 실행이다. 재시작한 fleetd가 이어받으면 자기 실행 ID로 바꾼다.
재시작한 fleetd는 이 retained 메시지로 운행을 이어받는다(8장).

```json
{"trip":"E1-k3x9ab-1","boot":"k3x9ab","elevator":"E1","stage":"exiting","origin":1,"dir":1,"stops":[3,5,7],"floor":5,
 "riders":[{"robot":"robot_2","cmd_id":"t-2.0.1.k3x9ab.R2","dest":7,"exit":7,"board":0,"slot":2,"status":"inside","accepted":true,"tries":0,"silence_s":69},
           {"robot":"robot_3","cmd_id":"t-3.0.1.k3x9ab.R2","dest":5,"exit":5,"board":1,"slot":1,"status":"exiting","accepted":true,"tries":0,"silence_s":63},
           {"robot":"robot_1","cmd_id":"t-1.0.1.k3x9ab.R2","dest":3,"exit":3,"board":2,"slot":0,"status":"out","accepted":true,"tries":0,"out_floor":3,"silence_s":51}],
 "turn":{"trip":"E1-k3x9ab-1","n":5,"robot":"robot_3","action":"exit","floor":5,"timeout_s":15,"deadline":"2026-11-03T09:00:41Z"},
 "n":5,"planned":"2026-11-03T09:00:03Z","lease":"2026-11-03T09:02:30Z"}
```

단계(`stage`): `planned`(명령 보냄·호출, 아무도 안 움직임) → `boarding` → `travel` → `exiting` → (`travel` → `exiting` …) → `done`.
탑승 전에 끝나면 `aborted`. 모든 정차가 끝났는데 확인되지 않은 로봇이 남으면 `blocked`(엘리베이터는 예약된 채로 둠).

로봇 상태(`riders[].status`):

| 상태 | 뜻 |
|---|---|
| `waiting` | 로비에서 탑승 차례를 기다림 |
| `boarding` / `exiting` | 지금 `turn`을 받아 문을 지나는 중 |
| `inside` | 탔음(`zone clear`) |
| `out` | 내렸음(`zone clear`, 또는 와이파이로 돌아와 확인) |
| `left` | 타지 않음(빠짐, 탑승 실패, 차례를 놓치고 로비에 있음) |
| `unsure_in` | 탑승 turn이 재시도까지 시간 초과: 탄 것으로 보고 계속(늦은 `zone`이나 와이파이 복귀로 확정) |
| `stuck` | 마지막 층에서도 못 내림: 사람이 필요. 엘리베이터는 묶여 있고 1분(`StuckRetry`)마다 그 층에서 다시 하차를 시도한다 |

`tries`는 지금 층에서 실패했거나 답이 없던 turn 수, `notified`는 그 로봇의 작업을 이미 다른 로봇에게 넘겼다는 표시.

`exit`는 지금 내릴 층이다. 못 내려서 다음 층으로 넘어가면(`carried`) `dest`와 달라진다.

### 3.9 차례 (`elevator/<id>/turn`)

```json
{"trip":"E1-k3x9ab-1","n":5,"robot":"robot_3","action":"exit","floor":5,"lobby":{"x":10,"y":0},"timeout_s":15,"deadline":"2026-11-03T09:00:41Z"}
```

- `n`은 운행 안에서 1씩 는다. **로봇은 자기 이름의 `turn`에만 움직인다.**
- 서버는 답이 올 때까지 5초마다 **같은 `n`으로** 다시 보낸다(turn이나 답이 유실돼도 이어지게).
- 이미 답한 `n`이 다시 오면 다시 움직이지 말고 **같은 결과를 다시 보낸다.**
- 움직이는 중(문 구역 안)에는 **어떤 turn에도 답하지 않는다**: 같은 `n`은 물론, 늦게 온 옛 `n`도 무시한다(옛 `n`의 답을 다시 보내면 엘리베이터가 그 로봇을 문 구역 수에서 빼 버린다).
  움직임이 끝나면 옛 `n`에는 다시 같은 답을 보낸다.
- 새 `n`인데 그 일을 이미 했으면(탔는데 `board`, 내렸는데 `exit`: 앞 답이 유실·지연돼 서버가 재시도한 경우) 움직이지 말고 `clear`로 답한다.
- 자기 `ride.trip`이 아닌 운행의 turn, `done`을 보낸 뒤의 turn은 무시한다.
- `exit` turn의 `lobby`가 그 층에서 나와 설 곳이다(넘겨져서 계획과 다른 층에서 내릴 때도).
- **차 안에서 exit turn이 안 오면 기다린다.** 스스로 내리지 않는다. 서버가 다시 보내거나(5초마다), 재시도·넘김을 정한다.
- `action: "board"`: 아래 4장의 탑승 순서. `"exit"`: 하차 순서. `floor`는 지금 문이 열린 층이다.
- `exit`의 `floor`가 `ride.exit_floor`와 달라도(다음 층으로 넘겨진 경우) 내린다.
- 아직 로비에 있는데(탑승 전) `exit` turn이 오면 움직이지 않고 `failed`(`reason: "not in the car"`)로 답한다.

### 3.10 문 구역 결과 (`elevator/<id>/zone`)

```json
{"trip":"E1-k3x9ab-1","n":5,"robot":"robot_3","action":"exit","event":"clear","floor":5,"seq":9,"stamp":1793696439.2}
{"trip":"E1-k3x9ab-1","n":5,"robot":"robot_3","action":"exit","event":"failed","floor":5,"reason":"door zone blocked","seq":9,"stamp":1793696439.2}
```

- `enter`: turn을 받아 문 구역에 들어가기 **직전에** 보낸다. 엘리베이터가 문 구역 수(`door_zone`)를 세는 데 쓴다. 서버는 판단에 쓰지 않는다.
- `clear`: 문 구역을 완전히 벗어났다(탈 때는 자기 칸, 내릴 때는 로비 쪽). 이 turn을 닫는다. 다음 로봇은 이것과 `door_zone: 0` 상태가 모두 와야 출발한다.
- `failed`: 지나가지 못했다(앞이 막힘 등). 탈 때는 로비에 그대로 있고, 내릴 때는 **자기 칸으로 돌아가 다른 로봇을 막지 않는다.**
- 엘리베이터 채널로 보낸다(차 안에서도 된다). `seq`/`stamp`는 기록용.

## 4. 순서와 시간

### 계획 (`elevator.PlanTrip`, 순수 함수)

1. 로봇이 로비에 도착하면 그 엘리베이터의 대기열에 들어간다.
2. 가장 오래 기다린 로봇이 `BatchWindow`(3초)를 채우고 엘리베이터가 비어 있으면(다른 운행 없음, 응답 없음 뒤 쉬는 중 아님) 계획한다.
   그 로봇의 층에서 기다리는 모두가 후보, 방향은 그 로봇의 방향.
3. 목적층 = 지금 층이면 제외. 반대 방향은 다음 운행. 칸 수를 넘으면 **가까운 층부터**(짧은 운행이 엘리베이터를 빨리 돌려준다), 거리가 같으면 이름순.
4. 하차 순서: 가까운 층부터, 같은 층이면 이름순. 칸 번호 = 하차 순서(0이 문 쪽). 탑승 순서 = 하차 순서의 반대.

예: 1층에서 3·7·5층 → 탑승 7·5·3, 칸 7→2, 5→1, 3→0(문 쪽). 정차 3, 5, 7.

### 진행

```
planned   ride 명령 전송 + call. 모든 로봇의 accepted(또는 상태의 task_id) + 문이 열린 상태를 기다림
boarding  탑승 순서대로 turn(board). 다음 turn은 앞 turn이 닫히고(clear/failed/시간 초과) door_zone 0 상태가 온 뒤.
          시간 초과는 같은 로봇 재시도(1번), 그래도 없으면 unsure_in. 5초마다 hold, 열린 turn은 5초마다 같은 n으로 재전송
travel    goto <다음 정차층>. 도착(문 열림)을 기다림. 늦으면 overdue 경보, 5초마다 같은 goto 재전송
exiting   안에 있을 수 있는 로봇 중 가장 문 쪽(칸 번호가 가장 작은) 로봇이 이 층에서 내리면 그 로봇에게 turn(exit).
          그 로봇이 여기서 내리지 않으면 이 층 하차는 끝(뒤 로봇은 지나갈 수 없음: 다음 층으로 넘김).
          실패·시간 초과는 재시도 1번, 그다음 넘김(마지막 층이면 stuck → blocked, 1분마다 재시도). 5초마다 hold
          모두 끝나면 다음 정차층으로 travel, 마지막이면 release → done
```

### 시간 (`elevator.Timing`, 기본값, `-elevators` 파일의 `"timing"`으로 바꿈)

| 이름 (파일 키) | 기본 | 뜻 |
|---|---|---|
| `BatchWindow` (`batch_window_s`) | 3초 | 첫 로봇 도착 후 함께 태울 로봇을 기다림 |
| `AcceptTimeout` (`accept_timeout_s`) | 10초 | 계획 후 이 안에 모두 수락 안 하면 중단. 그 사이 3초마다 같은 `cmd_id`로 재전송 |
| `CallMargin` (`call_margin_s`) | 15초 | 엘리베이터가 출발층에서 문을 열기까지 예상 시간 + 이만큼 |
| `Silence` (`silence_s`) | 5초 | 엘리베이터 상태가 이만큼 없으면 응답 없음(탑승 전이면 중단) |
| `TurnTimeout` (`turn_timeout_s`) | 15초 | `zone`이 없으면 그 turn을 닫고 같은 로봇의 실패한 시도로 센다(다음 로봇은 1장 ③대로 `door_zone: 0`을 본 뒤에만) |
| `ExitRetries` (`exit_retries`) | 1 | 하차 실패 시 같은 층에서 다시 주는 turn 수 |
| `HoldRefresh` / `HoldFor` | 5초 / 15초 | 문 유지 재요청 간격 / 한 번에 요청하는 시간 |
| `CmdRetry` (`cmd_retry_s`) | 5초 | `call`/`goto` 재전송 간격 |
| `TravelMargin` | 15초 | 이동 예상 + 이만큼 지나면 `overdue` |
| `LeaseMargin` | 60초 | 운행 전체 예상 + 이만큼 = 임대 |
| `SilenceMargin` | 20초 | 로봇별 끊김 예고에 더하는 여유 |
| `Backoff` | 30초 | 엘리베이터 응답 없음으로 중단한 뒤 다음 계획까지 |
| `TurnEstimate` | 6초 | 로봇 한 대가 문을 지나는 예상 시간(추정용: 끊김 예고·임대). 로봇이 느리면 키운다 |
| `StuckRetry` (`stuck_retry_s`) | 60초 | `stuck` 로봇의 하차 재시도 간격 |
| `Republish` (`republish_s`) | 10초 | retained 운행을 다시 보내는 간격 |

엘리베이터별 `floor_s`(층당 이동 초), `door_s`(문 열기/닫기 초)는 추정에만 쓴다.

### 끊김 예고(`silence_s`) 계산

로봇 i(탑승 순서 `board`, 칸 `slot`, 승객 n대)에 대해:

```
silence_s = (n - board) × TurnEstimate              # 자기 탑승 + 뒤에 타는 로봇들
          + Σ (출발층 → 자기 층까지 각 구간: |층 차| × floor_s + 2 × door_s)
          + (slot + 1) × TurnEstimate               # 자기 앞에서 내리는 로봇들 + 자기
          + SilenceMargin
```

1초 단위로 반올림. 넘겨진 로봇(다음 층 하차)은 예고보다 늦게 나올 수 있다: 그때는 `elevator_overdue` → STALE이고, 예고 끝 + 20초 안에 돌아오면 작업은 그대로 이어진다.

## 5. 로봇(어댑터)이 할 일 — 6b 구현 기준

```
IDLE ──ride 수락──▶ WAITING_ELEVATOR (로비, 와이파이 있음, elevator/<id>/turn 구독)
   turn(board, 나) ─▶ state{mode:BOARDING, silence_s} 보냄 → zone{board, enter} → 문 구역 진입(와이파이 곧 끊김)
                      자기 칸 도착 ─▶ zone{board, clear}   이벤트 boarded 쌓기          ▶ RIDING
                      못 들어감   ─▶ zone{board, failed} 로비로                         ▶ (서버가 cancel/재대기)
RIDING  turn(exit, 나, floor F) ─▶ zone{exit, enter} → 문 구역 → turn.lobby 쪽으로 나감
                      나옴        ─▶ zone{exit, clear, floor F}  이벤트 exited 쌓기      ▶ 와이파이 복귀 대기
                      막힘        ─▶ 자기 칸으로 돌아감, zone{exit, failed}  이벤트 exit_failed ▶ RIDING (다음 turn 기다림)
와이파이 복귀 ─▶ state{mode:IDLE, silence_s 없음} → 쌓인 event 전송 → cmd_ack{ride, done, floor}
```

- **차 안에서는 `fleet/*`로 아무것도 보내지 않는다**(보내도 닿지 않는다). 계획과 `turn`, 자기 상태 판단만으로 움직인다.
- 엘리베이터 채널은 차 안에서도 된다: `turn` 받기, `zone` 보내기.
- 자기 `turn`이 아니면 문 구역에 들어가지 않는다. 서버는 앞 로봇의 답과 엘리베이터의 `door_zone: 0`을 모두 받은 뒤에만 다음 turn을 주므로 "동시에 두 대"가 생기지 않는다
  (앞 로봇이 느려 시간 초과가 나도 마찬가지: 그 로봇이 문 구역에 있는 동안은 아무도 turn을 받지 않는다).
- 같은 `n`의 turn이 다시 오면 마지막 `zone`을 다시 보낸다(로봇은 `n` → 결과를 기억). 이미 한 일의 새 `n`이면 `clear`(3.9).
- 차 안에서 exit turn이 오지 않으면 그냥 기다린다(서버가 5초마다 다시 보내고, 시간 초과면 재시도·다음 층·사람 호출을 정한다). retained `trip`의 `turn`으로 움직이지 않는다.
- 내린 뒤 위치(`pose`)는 내린 층(`floor`)으로 보낸다. 엉뚱한 층에서 내렸으면 그 층을 그대로 보낸다(서버가 다시 계획한다).
- 쌓아 둔 이벤트는 seq를 붙인 채 그대로 보낸다(서버가 순서를 맞춘다).

mock 엘리베이터(6b)가 할 일: 3.6·3.7. 층 이동(`floor_s`), 문 열기·닫기(`door_s`), `hold_s` 만료, 같은 `cmd_id` 무시, 1초마다 상태,
`zone`을 구독해 `door_zone`을 세기(`enter` +1, 같은 로봇의 `clear`/`failed` -1, turn과 상관없이. 단 같은 운행에서 그 로봇의 마지막 `enter`보다 작은 `n`의 답은 빼지 않는다) — 0이 아니면 문을 닫지 않고,
서 있는 차의 문이 닫혀 있거나 닫히는 중이면 다시 열며, 0이 아니면 출발하지 않는다.
`hold`는 열린 문만 유지하고, 닫힌 문은 같은 층 `goto`로만 다시 연다. 실패 주입: 응답 없음, 문 막힘.

6b 구현(`cmd/mockelevator`, `adapter/fleet_logic.py`의 `RobotRide`)에서 정한 것 — 규약에는 영향 없음:
- mock 엘리베이터는 fleetd와 같은 `-elevators` 파일을 읽는다(`-floor-s`, `-door-s`로 덮어쓰기). 문 막힘은 `door_zone`에 1을 더한다(문이 닫히지 않고 지휘자는 turn을 주지 않음).
  실패는 `-unresponsive <id>`, `-door-blocked <id>` 또는 실행 중 `mockelevator/<id>/fault`에 `{"unresponsive":bool,"door_blocked":bool}`.
- 어댑터의 끊김은 탑승 turn에서 `BOARDING` 상태(+`silence_s`)를 보낸 직후 시작해, 하차 `clear` 1초 뒤 끝난다. 그동안 그 로봇의 `fleet/*`는 아무것도 보내지 않고 받은 명령은 버린다
  (실제로 닿지 않으므로). 와이파이가 돌아오면 `pose`(새 층) → `state`(`IDLE`, `silence_s` 0) → 쌓인 이벤트(seq 순) → `cmd_ack done`(+`floor`) 순서로 보낸다.
  `pose`를 먼저 보내는 것은 지휘자가 "와이파이로 ONLINE"을 볼 때 옛 층을 쓰지 않게 하기 위해서다.
- 탑승 실패(`zone{board, failed}`)면 로봇은 로비에 있으므로 끊김을 곧바로 끝내고 `WAITING_ELEVATOR`(그 ride `cmd_id`)를 다시 보고한다. 서버의 `cancel`로 끝난다.
- **움직임 시간 제한:** 어댑터는 `elevator_move`를 보낸 뒤 `turn_timeout_s` + 30초(`--move-timeout-extra`) 안에 로봇이 그 움직임을 끝냈다고 알리지 않으면 `failed`(`robot did not finish the move`)로 답한다
  (움직임 명령 유실, 로봇 프로그램 재시작). 탑승이면 끊김을 끝내고 `WAITING_ELEVATOR`를 보고한다. 단 로봇이 그 움직임을 아직 `running`으로 알리면(느림, 문 구역에서 고장으로 멈춤) 계속 기다린다:
  로봇이 문 구역에 있으므로 답하지 않는 것이 맞다(지휘자는 시간 초과로 재시도만 하고 아무도 움직이지 않는다).
- mock 로봇이 움직이는 중 고장(ERROR)이 나면 그 자리(문 구역일 수 있음)에 멈추고 움직임은 `running`으로 남는다. `reset_error`(사람이 손봄)가 움직임 시작점으로 되돌린 뒤에야 `failed`를 알린다.
- **운행이 로봇을 태운 채 끝나면**(차 안 `RIDING`인데 retained 운행이 비거나, 그 운행이 `done`/`aborted`가 되거나, 다른 운행으로 바뀌거나, 60초 동안 운행도 turn도 오지 않음):
  어댑터는 끊김을 끝내고 `pose`(차의 층) → `state` → 쌓인 이벤트 + `exit_failed`("still in the car, needs a human") → `cmd_ack failed`(+`floor`)를 보낸다.
  그 뒤 로봇이 차 밖으로 나올 때까지 `state`는 `mode: "ERROR"`, `error: "in elevator E1 without a ride: needs a human"`(배정되지 않게). 로봇이 차 밖이라고 알리면(`in_car: false`) `rescued` 이벤트와 평소 상태.
  어댑터가 다시 시작해서 차 안 로봇을 모를 때도 로봇이 `in_car: true`면 같은 ERROR로 보고한다.
- 운행 중에 사람이 로봇을 꺼내면(움직임 없이 `in_car: false`) `rescued` 이벤트를 쌓고 하차처럼 와이파이 복귀 → `done`(+그 층). 지휘자는 와이파이로 ONLINE이 된 것을 보고 밖으로 확인한다.
- 로봇은 어댑터가 보내는 `elevator_move`(board/floor/exit)로 움직인다(mock_robots/README.md). 차 안 위치는 로비에서 +x 쪽: 문 +1.0 m, 칸 0 +1.8 m, 칸마다 +0.8 m(대시보드도 같은 값으로 그린다).

## 6. 실패 처리

| 상황 | 감지 | 결과 |
|---|---|---|
| 로봇이 `ride`를 수락하지 않음 | `AcceptTimeout`(10초) | 운행 중단(`aborted`, 아무도 안 탐). 수락한 로봇은 `cancel` 받고 같은 로비에서 다시 대기(시도 횟수 그대로). 안 한 로봇의 작업은 시도 1번 쓰고 대기열로 |
| 엘리베이터 응답 없음(탑승 전) | 상태 5초 없음, 또는 출발층에서 제때 문이 안 열림 | 운행 중단, 모든 로봇 `cancel` 후 다시 대기(시도 안 씀), 엘리베이터 30초 쉼 |
| 엘리베이터 응답 없음(탄 뒤) | 이동 예상 + 15초 | `overdue` 경보. 같은 `goto` 계속 재전송. 로봇이 안에 있으므로 중단하지 않는다 |
| 탑승 실패 | `zone{board, failed}` | 그 로봇만 빠짐(`left`), 시도 1번 쓰고 작업 대기열로. 운행은 나머지로 계속 |
| 탑승 전 로봇 OFFLINE·ERROR | 레지스트리 | 그 로봇만 빠짐, 작업은 시도 1번 쓰고 대기열로(지켜보기로 옛 ride 취소) |
| 하차 실패 | `zone{exit, failed}` | 같은 층에서 한 번 더 turn(`ExitRetries`). 또 실패하면 그 로봇과 **그 층에서 내릴 예정이던 뒤 로봇들**(지나갈 수 없음)을 다음 정차층으로 넘김(`replanned`). 거기서 문 쪽에 있으므로 먼저 내린다 |
| 마지막 층에서 하차 실패 | 넘길 층 없음 | `exit_failed`("needs a human"), 상태 `stuck`, 문 놓음(`blocked`). 그 로봇의 작업은 다른 로봇에게(시도 1번, 한 번만). **엘리베이터는 예약된 채로**(다른 운행 없음), 1분마다 그 층에서 문을 다시 열고 하차 turn을 다시 준다. 끝나는 길: 그 로봇이 밖에서 확인(`zone clear`, 와이파이로 ONLINE, `done`) 또는 운영자의 `POST /elevators/{id}/release` |
| `zone` 없이 turn 시간 초과 | `TurnTimeout`(그 사이 5초마다 같은 `n` 재전송) | `overdue`. **다음 로봇은 움직이지 않는다.** 실패한 시도로 센다: 하차는 "아직 안에 있음" → 재시도 → 다음 층으로 넘김(뒤 로봇도) → 마지막 층이면 `stuck`. 탑승은 재시도 → 그래도 없으면 `unsure_in`(탄 것으로 봄). 재시도든 다음 로봇이든 `door_zone: 0`이 와야 나간다. 늦은 `clear`는 받아들인다. 늦은 `failed`(하차)는 이미 "안에 있음"으로 쳤으므로 무시 |
| 문 구역이 비지 않음 | `door_zone > 0`이 계속 | 아무도 움직이지 않고 turn 재시도·`overdue`만. 사람이 치우면 이어진다 |
| 문이 닫힘(유지 만료, 재시작) | 상태의 `door` | turn을 주지 않고 같은 층 `goto`로 다시 연다 |
| 엘리베이터가 탑승·하차 중 무응답 | 상태 5초 없음 | `overdue`("car not answering"), 아무도 안 움직임, 같은 층 `goto` 재전송 |
| 엉뚱한 층에서 내림 | `done`의 `floor`(없으면 `pose`)가 작업 지점 층과 다름 | `replanned`: 그 층의 로비 → 다시 엘리베이터 → 지점. 시도 안 씀 |
| 와이파이가 예고보다 늦게 돌아옴 | 레지스트리 | `elevator_overdue` → STALE. 20초 안에 돌아오면 그대로 진행 |
| 와이파이가 훨씬 늦게(나온 뒤) | OFFLINE | 내린 것은 확인됐으므로 평소 규칙: 시도 1번, 다른 로봇에게(지켜보기). 돌아온 로봇은 아무것도 안 함 |
| 와이파이가 안 돌아옴(안에 갇힘 의심) | 로봇 레지스트리의 `elevator_overdue` + 운행 임대 만료 | `overdue`("lease expired") 경보. 로봇이 안에 있는 동안 작업은 그 로봇에(두 로봇이 같은 작업을 하지 않게) |
| 임대(lease) 만료 | 예상 + 60초 | 탑승 전이면 중단(안전). 탄 뒤면 `overdue` 경보와 함께 밖으로 확인되지 않은 로봇마다 `stuck` 처리(작업은 다른 로봇에게, 한 번만). 엘리베이터는 **그 로봇들이 확인되거나 운영자가 놓을 때까지** 예약된 채로 |
| 운영자 강제 반환 | `POST /elevators/{id}/release` | `release` 명령, 운행 끝(`released`, "by operator"), retained 운행 지움. 탑승 전 로봇은 `cancel` 후 다시 대기, 밖으로 확인되지 않은 로봇의 작업은 시도 없이 대기열로 + 지켜보기(`cancel`). 차 안에 남은 로봇의 어댑터는 운행이 끝난 것을 보고 끊김을 끝내며 ride는 `failed`, 로봇은 꺼낼 때까지 ERROR("needs a human")로 보고한다(5장) |
| 갇힌 로봇 꺼내기(운영자 절차) | 대시보드 경보 "갇힘"/"막힘", 로봇 IN_ELEVATOR→STALE | ① 대시보드에서 차·층·칸 확인 ② 사람이 로봇을 꺼내 그 층 로비에 둔다(mock: 로봇의 `rescue` 서비스, 와이파이가 돌아온 뒤면 `fleet/<robot>/cmd` `{"type":"rescue"}`) — 운행 중에 꺼내면 로봇이 `done`(+층)을 보내 운행이 스스로 끝난다 ③ 운행이 아직 묶여 있으면 "강제 반환"(`POST /elevators/{id}/release`) ④ 로봇이 ERROR로 남아 있으면 꺼낸 뒤 `rescued` 이벤트와 함께 대기로 돌아오는지 확인. 고장 난 로봇이면 `reset_error` |
| 대기 로봇의 목적층이 지금 층 | 계획 때 | 타지 않고 그 구간을 그냥 `goto`로 끝낸다 |
| 배터리 위험(10% 미만) | 배터리 정책 | 로비에서 대기 중이거나 `planned` 단계(아직 아무도 안 움직임)면 작업을 돌려주고(시도 안 씀) 도크로. **탑승이 시작되면 돌려받지 않는다**: 내린 뒤 지점으로 가는 `goto`에서 평소처럼 돌려준다. 타고 있는 로봇에게 `dock`을 보내지 않는다 |
| 사용자 취소 | API | 대기 중·탑승 전이면 빠지고 `cancel`. 문을 지나기 시작했거나 안에 있으면 운행은 그 로봇을 계획대로 내려 준다(작업만 CANCELLED) |
| 서버 재시작 | 8장 | |

## 7. 이벤트 (`/events`, `/ws`, `type: "elevator"`)

모든 엘리베이터 이벤트는 `elevator`(엘리베이터 ID), `robot`(해당되면), `task`(그 로봇의 진행 중 작업), `detail`(`<운행 ID>: ...`)을 가진다.

| `kind` | 언제 |
|---|---|
| `trip_planned` | 계획함. `detail`: `from 1 up: board robot_2,robot_3,robot_1; stops [3 5 7]` (+ `next trip ...`) |
| `called` | `call` 보냄 |
| `boarding` | 탑승 turn 줌(로봇) |
| `boarded` | `zone{board, clear}` (늦게 온 것 포함) |
| `departed` | 다음 정차층으로 `goto` |
| `stop` | 정차층 도착(문 열림) |
| `exiting` | 하차 turn 줌(로봇) |
| `exited` | 하차 확인(`zone clear`, 와이파이 복귀, `done`) |
| `exit_failed` | 하차 실패(재시도 / 포기 / needs a human) |
| `replanned` | 다음 층으로 넘김, 엉뚱한 층에서 나와 다시 계획, 재시작 후 운행 이어받음 |
| `aborted` | 운행 중단(사유), 또는 한 로봇이 빠짐 |
| `overdue` | turn 시간 초과(재시도·넘김), 이동 지연, 탑승·하차 중 엘리베이터 무응답, 확인 안 된 로봇이 남아 엘리베이터 유지, 임대 만료 |
| `released` | 운행 끝, 엘리베이터 반환(운영자 반환 포함) |
| `robot_event` | 로봇이 보낸 이벤트(seq 순서로 기록) |

로봇 연결 이벤트(`type: "conn"`의 `IN_ELEVATOR`, `type: "elevator_overdue"`)는 2단계 그대로다.

## 8. 서버 재시작

**결정: 운행 상태는 retained `elevator/<id>/trip`으로 이어받는다.** SQLite에는 작업의 단계(`phase`, `elevator`, `trip`, `from`, `hops`)만 저장한다.

- 지휘자는 단계가 바뀔 때마다(+10초마다, 재연결 때) 운행 전체를 retained로 보낸다. 재시작한 fleetd는 구독 직후 그것을 받는다.
- 메아리 막기: 이어받은 운행의 `boot`를 자기 실행 ID로 바꾸고, 이 실행에서 끝난 운행 ID를 기억해 무시하며, 끝난 운행의 retained 메시지는 지운다.
  그래서 자기 메시지가 돌아오거나(같은 `boot`) 끝난 운행의 메시지가 늦게 와도 다시 받아들이지 않는다.
- `planned`(아무도 안 움직임)였으면 **안전하게 중단**: 탑승 전 로봇에게 `cancel`, 같은 로비에서 다시 대기.
- `boarding`/`travel`/`exiting`/`blocked`였으면 **이어서 진행**: 시계(turn 시간 제한, 임대)는 새로 시작, 같은 층 `goto`로 문을 다시 열고,
  **문이 열린 것을 확인한 뒤에** 진행 중이던 turn을 **같은 `n`으로 다시 보낸다.** 이미 답한 로봇은 같은 결과를 다시 보내므로 이어진다. 서버가 꺼진 동안 보낸 `zone`은 잃을 수 있지만 그 경우 turn 시간 초과 규칙대로 간다.
- 작업 쪽: 단계가 `ride`인 작업은 운행을 이어받으면 바로 확인된다(로봇이 차 안이라 보이지 않아도 재배정하지 않음).
  이어받을 운행이 없으면(retained 메시지 없음) 평소 재시작 규칙: 유예(25초) 동안 기다리며 같은 `ride` `cmd_id`로 한 번 물어(끝났으면 어댑터가 `done`을 다시 보냄),
  로봇이 아직 그 계획을 들고 있으면 `cancel` 후 다시 대기, 끝내 확인 안 되면 대기열로 + 지켜보기.
- 단계가 `wait`(로비 대기)인 작업은 로봇이 그 층에서 ONLINE·IDLE·`task_id` 없음으로 보이면 다시 대기열에 넣는다.
- 한계: 브로커(Mosquitto)가 retained 메시지를 잃으면(영속 저장 꺼짐 + 브로커 재시작) 탑승 중이던 운행은 이어받지 못한다. 차 안 로봇은 turn이 오지 않아 갇히고
  `elevator_overdue` → OFFLINE → 작업 재배정(지켜보기) + 사람이 필요하다.

## 9. 시퀀스

### 정상: 1층에서 3·7·5층 (robot_1→3, robot_2→7, robot_3→5)

```
robot_1/2/3       fleetd(지휘자)                     E1
  │ goto 로비(10,0) ◀──│                              │
  │ done ─────────────▶│ 대기열 (3초 모음)              │
  │                    │ plan: 탑승 2,3,1 / 칸 2,1,0   │
  │ ride(plan) ◀───────│── call 1 (hold 15) ─────────▶│
  │ accepted ─────────▶│                              │ 1층 도착, 문 열림
  │                    │◀──────── state{1, open, trip}│
  │                    │ 모두 수락 + 문 열림 → boarding │
  │ turn#1 board robot_2 ◀─(elevator 채널)             │
robot_2: state{BOARDING, silence_s:69} → 탑승 → zone#1 clear ─▶ │
  │ turn#2 board robot_3 ◀─     (5초마다 hold) ─────────▶│
robot_3: ... zone#2 clear ─▶                                    │
  │ turn#3 board robot_1 ◀─                                     │
robot_1: ... zone#3 clear ─▶ │── goto 3 ─────────────▶│ 문 닫힘, 이동
  │                    │◀──────── state{3, open, trip}│
  │ turn#4 exit robot_1 @3 ◀─ (문 쪽 칸 0)               │
robot_1: 나옴 → zone#4 clear ─▶ │── goto 5 ───────────▶│
robot_1: (와이파이) state{IDLE}, event #.. , ack ride done floor 3 ─▶ goto 지점
  │ turn#5 exit robot_3 @5 ... zone#5 clear ─▶ │── goto 7 ─▶│
  │ turn#6 exit robot_2 @7 ... zone#6 clear ─▶ │── release ─▶│
  │                    │ released                      │
```

### 하차 실패 → 다음 층으로

```
@3: turn#4 exit robot_1 → zone#4 failed (칸으로 돌아감)   exit_failed (try 1; retrying)
    (door_zone 0 상태를 받은 뒤)
    turn#5 exit robot_1 → zone#5 failed                  exit_failed (gave up) + replanned: robot_1 exits at 5
    goto 5
@5: turn#6 exit robot_1 (칸 0, 문 쪽) → clear             exited
    turn#7 exit robot_3 → clear
robot_1 와이파이 복귀: ack done floor 5 → 작업 지점은 3층 → replanned: 5층 로비 → 다음 운행(아래로) → 3층
```

### 엘리베이터 응답 없음 (탑승 전)

```
plan → ride 명령, call 1 → (상태 없음 5초) → aborted "elevator not responding"
     → 각 로봇 cancel(target: <ride>) → ride cancelled, 로봇은 로비에 그대로 → 다시 대기
     → 30초 쉼 → 다시 plan
```

### 재시작 (탑승 중)

```
fleetd 종료 (robot_2, robot_3 안, stage travel → 5층)
fleetd 시작 → 구독 → retained trip{stage: travel, floor 5} → replanned "trip restored"(boot를 새 실행으로) → goto 5 다시
            → 작업 phase ride는 운행이 있으므로 확인됨 (25초 유예가 지나도 재배정 안 함)
E1 state{5, open} → exiting → turn exit ... (이후 정상)
```

## 10. `GET /elevators`

```json
[{"id":"E1","capacity":3,"floors":[1,2,3,4,5,6,7,8,9,10],"lobby":{"x":10,"y":0},
  "state":{"boot":"car-7","seq":812,"floor":5,"dir":"idle","door":"open","moving":false,"hold":true,"trip":"E1-k3x9ab-1","load":2,"cmd_id":"...","door_zone":0},
  "heard":"2026-11-03T09:00:40.1+09:00","responding":true,
  "trip":{ ...3.8과 같음... },
  "waiting":[{"robot":"robot_4","floor":1,"dest":6,"since":"2026-11-03T09:00:38+09:00"}],
  "backoff_until":"..."}]
```

`state`는 상태를 받기 전 `null`, `trip`은 운행이 없으면 `null`, `backoff_until`은 쉬는 중일 때만. 대시보드(6b)는 이것을 1초마다 읽거나
`type: "elevator"` 이벤트가 올 때 읽으면 된다(`/ws/live` 묶음에는 엘리베이터를 넣지 않았다).

`POST /elevators/{id}/release`(본문 `{}`, `Content-Type: application/json`, 다른 POST와 같은 사이트 검사): 운영자 강제 반환(6장).
`200` + 반환 뒤 엘리베이터, 없는 엘리베이터 `404`, 운행이 없으면 `409`, 형식·사이트 위반 `415`/`403`.

## 11. 설정 (`-elevators`)

```json
{"elevators":[{"id":"E1","capacity":3,"floors":[1,2,3,4,5,6,7,8,9,10],
               "lobby":{"x":10,"y":0},"lobbies":{"3":{"x":11,"y":1}},"floor_s":2,"door_s":2}],
 "timing":{"batch_window_s":3,"turn_timeout_s":15}}
```

- timing 값은 모두 0보다 커야 한다(`exit_retries`는 0 이상). `hold_refresh_s`는 `hold_for_s`보다 짧아야 한다(아니면 두 `hold` 사이에 문이 닫힌다). 어기면 fleetd가 시작하지 않는다.

- 플래그를 주지 않으면 위의 E1 하나(로비는 모든 층 (10, 0), 층마다 2초, 문 2초). `-elevators none`이면 엘리베이터 없음: 다른 층 구간도 예전처럼 그냥 `goto`.
- `id`·`floors`(2개 이상)·`lobby` 필수, `capacity`(기본 3)·`floor_s`·`door_s`(기본 2) 선택. `lobbies`는 층별 로비(키는 층 번호 문자열).
- 다른 층 구간은 두 층을 모두 서는 엘리베이터 중 ID가 가장 앞인 것을 쓴다. 없으면 작업은 바로 `FAILED`("no elevator serves floors X and Y").

## 12. 알려진 한계

- 엘리베이터 여러 대 사이의 최적 배정은 하지 않는다(PLAN 8장). 사람 승객도 고려하지 않는다.
- 로비의 대기 자리는 하나의 점이다. 여러 로봇이 같은 점으로 간다(실제로는 Nav2가 비켜 서거나 대기 자리를 여러 개 두어야 한다).
- 한 칸 줄(single file) 모델: 앞 로봇이 못 내리면 같은 층에서 내릴 뒤 로봇도 넘긴다. 실제 칸이 2열이면 더 느슨하게 할 수 있다.
- 차 안 로봇의 와이파이는 항상 끊긴다고 가정한다: "와이파이로 ONLINE이면 차 밖"으로 판단한다.
- 운행 상태는 retained 메시지에만 있다(8장).
- fleetd의 발행 대기열: 로봇 명령은 512개까지(넘으면 거절, 서버가 재전송), 엘리베이터 명령·turn은 거절하지 않고 2,048개를 넘으면 가장 오래된 것을 버린다(지휘자가 재전송),
  retained 운행은 버리지 않고 같은 엘리베이터의 새 것으로 대체, 발행이 실패하면 1초 뒤 다시 보낸다. 그래서 브로커가 잠시 끊겨도 마지막 운행 상태가 남는다.
