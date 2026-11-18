# 로봇 프로토콜 (fleetd가 관리하는 로봇이 지켜야 할 것)

fleetd에 로봇을 붙이는 두 가지 방법이 있다.

1. **ROS 2 로봇 + 어댑터**: `mock_robots/mock_fleet.py`가 ROS 2 토픽·서비스만 말하고, `adapter/fleet_adapter.py`가 그것을 MQTT로 옮긴다.
2. **MQTT를 직접 말하는 로봇**: 로봇(또는 로봇 여러 대를 돌리는 프로세스) 자신이 이 문서대로 MQTT를 말한다. ROS도 어댑터도 없다.
   `unity/FleetUnity`(Unity 시뮬레이터)가 이 방식이다.

fleetd는 두 종류를 구분하지 않는다. 같은 토픽에 같은 JSON이 오면 같은 로봇이다. 이 문서는 **엘리베이터를 타지 않는** 로봇에게 필요한 전부를 적는다.
엘리베이터 탑승(`ride` 명령, `elevator/<id>/*` 채널, `fleet/<robot>/event`)은 [ELEVATOR.md](ELEVATOR.md)에 있다.
Unity 로봇도 ROS mock 로봇과 똑같이 엘리베이터를 탄다. ROS 로봇과 Unity 로봇이 한 차에 함께 탈 수 있다(9.1절).

이 문서는 코드에서 옮겨 적었다. 규칙마다 근거를 `파일:줄`로 단다. 코드와 이 문서가 다르면 코드가 기준이다.
`internal/` 줄 번호는 2026-11-18 작업 트리 기준이다(그 시점에 `can_ride` 변경이 커밋 전 상태로 들어와 있었다). 줄이 밀렸으면 함수 이름으로 찾는다.

## 1. 한눈에 보기

| 토픽 | 방향 | QoS | retained | 내용 | 근거 |
|---|---|---|---|---|---|
| `fleet/<robot>/pose` | 로봇 → fleetd | 0 | 아니오 | 위치·자세·층. 최대 5 Hz, 데드밴드, 정지 시 1 s 하트비트 | `adapter/fleet_adapter.py:284-296`, `cmd/fleetd/main.go:160` |
| `fleet/<robot>/state` | 로봇 → fleetd | 1 | 아니오 | 모드·배터리·task_id·오류. 바뀌면 즉시, 아니면 1 s 하트비트 | `adapter/fleet_adapter.py:303-325`, `cmd/fleetd/main.go:161` |
| `fleet/<robot>/cmd` | fleetd → 로봇 | 1 | 아니오 | `goto` / `cancel` / `dock` (+ `ride`, ELEVATOR.md) | `cmd/fleetd/outbox.go:147`, `cmd/fleetd/outbox.go:187`, `adapter/fleet_adapter.py:171` |
| `fleet/<robot>/cmd_ack` | 로봇 → fleetd | 1 | 아니오 | `accepted` / `done` / `failed` / `cancelled` | `adapter/fleet_adapter.py:203-204`, `cmd/fleetd/main.go:28`, `cmd/fleetd/main.go:163` |
| `fleet/<robot>/event` | 로봇 → fleetd | 1 | 아니오 | 로봇이 겪은 일(seq + 로봇 시각). **엘리베이터 탑승에서만** 나온다. 타지 않는 로봇은 보내지 않아도 된다. 형식은 ELEVATOR.md | `adapter/fleet_adapter.py:229-233`, `adapter/fleet_logic.py:534-541`, `internal/elevator/proto.go:22`, `cmd/fleetd/main.go:167`, `cmd/fleetd/main.go:173-181` |
| `fleet/adapter/<host>/status` | 로봇 프로세스 → fleetd | 1 | **예** | 연결 시 `online`, LWT와 정상 종료 시 `offline` | `adapter/fleet_adapter.py:141-145`, `adapter/fleet_adapter.py:180`, `adapter/fleet_adapter.py:425-430`, `cmd/fleetd/main.go:162` |

- `<robot>`은 로봇 이름이다. 토픽 한 단계이므로 `/`, `+`, `#`이 들어가면 안 되고, `adapter`도 안 된다(`cmd/fleetd/main.go:256-261`, `internal/fleet/ingest.go:23`).
  이름은 브로커 전체에서 유일해야 한다. 같은 이름의 로봇 둘은 fleetd에게 한 로봇으로 보인다.
- `<host>`는 MQTT 연결 하나(어댑터 프로세스 하나)의 이름이다. 그 연결로 말하는 모든 로봇의 pose·state에 같은 값을 `host`로 넣는다.
  fleetd는 `offline`을 받으면 `host`가 그 값인 로봇을 모두 OFFLINE으로 만든다(`internal/fleet/registry.go:261-266`). `host`가 다르면 LWT가 로봇에 닿지 않는다.
- MQTT keepalive는 5초다(어댑터 `adapter/fleet_adapter.py:66`, `adapter/fleet_adapter.py:151`; fleetd 자신도 5초 `internal/mqttx/mqttx.go:71`).
  연결이 소리 없이 죽어도 브로커가 1.5 × keepalive ≈ 7.5초 안에 LWT를 낸다. 프로세스가 죽어 TCP가 끊기면 즉시 낸다.
- 시각: `stamp`는 보낸 쪽 벽시계의 유닉스 초(소수)다. fleetd의 생존 판단은 **fleetd가 받은 시각**만 쓴다(`internal/fleet/registry.go:105-107`의 `LastSeen` 주석).
  `expires_at`만 로봇 벽시계와 비교한다(4.2절). 그래서 로봇 시계는 NTP 정도로 맞아 있어야 한다.

## 2. 연결과 호스트 상태 (`fleet/adapter/<host>/status`)

1. 연결할 때 LWT를 건다: 토픽 `fleet/adapter/<host>/status`, 내용 `{"host":"<host>","boot":"<boot>","status":"offline"}`, QoS 1, **retained** (`adapter/fleet_adapter.py:145`, `adapter/fleet_adapter.py:161-162`).
2. 연결될 **때마다**(재연결 포함) 같은 토픽에 `"status":"online"`을 QoS 1 retained로 보내고, 맡은 로봇마다 `fleet/<robot>/cmd`를 QoS 1로 다시 구독한다(`adapter/fleet_adapter.py:175-182`).
   재연결 간격은 흩뜨린다. 어댑터는 0.5~1.5 s에서 시작해 최대 30 s까지 늘린다(`adapter/fleet_adapter.py:146-147`).
3. 정상 종료할 때는 LWT에 맡기지 않고 직접 `offline`을 보낸 뒤 끊는다(`adapter/fleet_adapter.py:425-430`).
4. fleetd는 이 토픽을 `fleet/adapter/+/status`(QoS 1)로 구독하고(`internal/fleet/ingest.go:13`, `cmd/fleetd/main.go:162`),
   `status`가 `"offline"`일 때만 반응한다. 그 `host`의 로봇을 모두 즉시 OFFLINE으로 바꾸고 `adapter` 이벤트를 남긴다(`internal/fleet/ingest.go:23-35`, `internal/fleet/registry.go:261-274`).
   `online`은 기록만 한다. 로봇은 자기 메시지로 다시 ONLINE이 된다(5절).
5. MQTT 콜백은 MQTT 스레드에서 온다. 어댑터는 받은 메시지를 큐(inbox)에 넣고 주 루프에서 꺼내 처리한다(`adapter/fleet_adapter.py:140`, `adapter/fleet_adapter.py:189-190`, `adapter/fleet_adapter.py:328-337`).
   로봇 상태를 한 스레드만 만지게 하려는 것이다. 직접 말하는 로봇도 같은 구조를 권한다.

## 3. 로봇 → fleetd

### 3.1 `boot`와 `seq` (모든 pose·state)

- `boot`: 송신 프로세스가 시작할 때마다 새로 만드는 문자열(어댑터는 `uuid4().hex[:8]`, `adapter/fleet_adapter.py:135`).
- `seq`: **로봇별, 토픽별**로 1씩 늘어나는 정수(pose와 state가 따로 센다, `adapter/fleet_adapter.py:80-81`, `adapter/fleet_adapter.py:290`, `adapter/fleet_adapter.py:317`).
- fleetd는 `boot`가 지난번과 다르거나 `seq`가 지난번보다 클 때만 받아들인다(`internal/fleet/registry.go:176-177`). 나머지(늦게 온 것, 중복)는 조용히 버린다(`internal/fleet/ingest.go:43-52`).
  그래서 재시작한 로봇은 **반드시 새 `boot`**로 보내야 한다. 같은 `boot`로 seq를 1부터 다시 세면 지난번 seq를 넘을 때까지 전부 버려진다.
- 중간에 seq가 비는 것(보내지 못한 번호)은 괜찮다. 늘어나기만 하면 된다.

### 3.2 `fleet/<robot>/pose` (QoS 0)

```json
{"boot":"a470931a","seq":42,"stamp":1794989140.517,"host":"unity-1",
 "frame":"map","floor":1,"x":2.907,"y":0.226,"yaw":0.785,"v":0.8}
```

| 필드 | 타입 | 뜻 / 제한 | 근거 |
|---|---|---|---|
| `boot`, `seq`, `stamp`, `host` | 3.1절 | | `internal/fleet/registry.go:12-23` |
| `frame` | 문자열 | 좌표계 이름. 어댑터는 `"map"` | `adapter/fleet_adapter.py:294` |
| `floor` | 정수 | 로봇이 있는 층. **빠지면 0**이 되어 1층 작업도 "다른 층"으로 보인다. 반드시 넣는다 | `internal/fleet/registry.go:18`, `adapter/fleet_adapter.py:294` |
| `x`, `y` | 실수, m | 유한하고 \|x\|,\|y\| ≤ 1e6 | `internal/fleet/check.go:13`, `internal/fleet/check.go:23-24` |
| `yaw` | 실수, rad | 유한. +x에서 반시계 방향 | `internal/fleet/check.go:25-26` |
| `v` | 실수, m/s | 유한하고 \|v\| ≤ 100 | `internal/fleet/check.go:14`, `internal/fleet/check.go:27-28` |

제한을 벗어난 메시지는 **통째로** 버려지고(`rejected` 카운터가 늘어남) 로봇은 마지막으로 받은 좋은 값을 유지한다(`internal/fleet/check.go:8-11`, `internal/fleet/ingest.go:55-58`).
첫 pose를 받기 전에는 `has_pose`가 false이고, 그런 로봇에는 작업을 주지 않는다(`internal/fleet/registry.go:95-97`, `internal/task/manager.go:587`).

보내는 규칙(어댑터와 같게):
- 0.2 s 타이머마다(최대 5 Hz) 검사한다(`adapter/fleet_adapter.py:155`, `adapter/fleet_logic.py:10`).
- 지난번 보낸 것보다 **5 cm 이상 움직였거나 2° 이상 돌았으면** 보내고, 아니면 **1 s마다** 하트비트로 보낸다. 처음 한 번은 무조건 보낸다
  (`adapter/fleet_logic.py:11-13`, `adapter/fleet_logic.py:25-32`). 각도 차이는 ±π를 넘어가는 것을 고려해 잰다(`adapter/fleet_logic.py:21-22`).
- 값은 소수 셋째 자리로 반올림한다(`adapter/fleet_adapter.py:295`).

### 3.3 `fleet/<robot>/state` (QoS 1)

```json
{"boot":"a470931a","seq":17,"stamp":1794989140.533,"host":"unity-1",
 "mode":"MOVING","battery":87.4,"task_id":"t-2.0.1.oywypl","error":"","silence_s":0,"can_ride":true}
```

| 필드 | 타입 | 뜻 / 제한 | 근거 |
|---|---|---|---|
| `boot`, `seq`, `stamp`, `host` | 3.1절 | | `internal/fleet/registry.go:26-30` |
| `mode` | 문자열 | 6절 | `internal/fleet/registry.go:31` |
| `battery` | 실수 또는 `null` | %, 0..100. 범위를 벗어나거나 NaN이면 메시지 전체가 버려진다. `null`이나 누락은 "모름" | `internal/fleet/registry.go:32`, `internal/fleet/check.go:36-37` |
| `task_id` | 문자열 | 지금 몰고 있는 `goto`의 `cmd_id`. 없으면 `""` (7.4절) | `adapter/fleet_adapter.py:311`, `adapter/fleet_adapter.py:323` |
| `error` | 문자열 | 오류 설명. 없으면 `""` | `adapter/fleet_adapter.py:313` |
| `silence_s` | 실수 | 예고한 통신 끊김(초, 0..3600). 엘리베이터 전용. 타지 않는 로봇은 항상 0 | `internal/fleet/registry.go:36-38`, `internal/fleet/check.go:15`, `internal/fleet/check.go:38-39` |
| `can_ride` | bool, 선택 | 엘리베이터를 탈 수 있는가. **빠지면 true** (8절) | `internal/fleet/registry.go:40-42`, `internal/fleet/registry.go:238` |

보내는 규칙:
- 0.1 s마다 검사한다(`adapter/fleet_adapter.py:156`). `(mode, error, task_id, silence_s)`가 바뀌면 **즉시**, 아니면 **1 s마다** 보낸다
  (`adapter/fleet_adapter.py:314-315`, `adapter/fleet_logic.py:14`, `adapter/fleet_logic.py:35-41`). 배터리만 바뀐 것은 하트비트에 실려 간다.
- `task_id`가 바뀌는 순간(명령 수락, 완료, 취소)이 즉시 나가는 것이 중요하다. fleetd는 이것으로 진행을 확인하고(`internal/task/manager.go:418-420`),
  `task_id`가 비어 있지 않은 로봇에는 새 작업을 주지 않는다(`internal/task/manager.go:593`).
- 어댑터는 로봇 자신의 상태가 1.5 s 넘게 끊기면 pose·state를 멈춘다. 오래된 값을 대신 보내 침묵을 숨기지 않기 위해서다(`adapter/fleet_logic.py:15`, `adapter/fleet_logic.py:44-47`, `adapter/fleet_adapter.py:285`, `adapter/fleet_adapter.py:306`).
  직접 말하는 로봇도 같다. 시뮬레이션이 멈췄으면 보내지 않는 편이 맞다.

## 4. fleetd → 로봇: 명령 (`fleet/<robot>/cmd`, QoS 1)

### 4.1 형식

fleetd는 항상 같은 구조체를 JSON으로 보낸다(`internal/task/task.go:90-104`). 쓰지 않는 필드도 0으로 들어온다.

```json
{"cmd_id":"t-2.0.1.oywypl","type":"goto","x":15,"y":8,"yaw":0,"expires_at":1794989150.517}
{"cmd_id":"t-2.0.1.oywypl.x1794989140517","type":"cancel","x":0,"y":0,"yaw":0,"expires_at":1794989150.517,"target":"t-2.0.1.oywypl"}
{"cmd_id":"dock.unity_1.1.oywypl","type":"dock","x":0,"y":0,"yaw":0,"expires_at":1794989150.763}
```

| 필드 | 뜻 | 근거 |
|---|---|---|
| `cmd_id` | 명령 ID. fleetd가 한 번 쓴 ID는 다시 쓰지 않는다. goto는 `<작업>.<구간>.<배정 횟수>.<서버 boot>`, cancel은 `<대상>.x<밀리초>`, dock은 `dock.<로봇>.<번호>.<서버 boot>` | `internal/task/manager.go:712-713`, `internal/task/manager.go:738-741`, `internal/task/battery.go:239` |
| `type` | `goto`, `cancel`, `dock` (+ `ride`) | `internal/task/task.go:92` |
| `x`, `y`, `yaw` | goto 목표. 층은 없다: 로봇의 **현재 층** 위의 점이다 | `internal/task/manager.go:731-733` |
| `expires_at` | 유닉스 초. 보낸 시각 + 10 s | `internal/task/task.go:96`, `internal/task/manager.go:57`, `internal/task/manager.go:750-751` |
| `target` | cancel이 겨냥하는 `cmd_id` | `internal/task/task.go:97-100`, `internal/task/manager.go:741` |

`cmd_id`나 `type`이 없거나 JSON이 깨진 명령은 경고만 남기고 버린다(응답 없음, `adapter/fleet_adapter.py:348-353`).

### 4.2 중복과 만료 (모든 명령에 먼저 적용)

어댑터의 `CommandBook`(`adapter/fleet_logic.py:50-81`, 사용 `adapter/fleet_adapter.py:355-365`):

1. 처리한 `cmd_id`와 **그 명령에 마지막으로 보낸 ack**를 최근 256개까지 기억한다(`adapter/fleet_logic.py:60-62`).
2. 이미 본 `cmd_id`면 **다시 실행하지 않고**, 기억해 둔 마지막 ack를 그대로 다시 보낸다. 아직 ack가 없으면 아무것도 보내지 않는다
   (`adapter/fleet_logic.py:65-66`, `adapter/fleet_logic.py:78-81`, `adapter/fleet_adapter.py:356-362`).
   이유: QoS 1은 두 번 배달할 수 있고, fleetd는 ack를 못 받으면 **같은 `cmd_id`로** 다시 보낸다(`internal/task/manager.go:422-436`). 다시 보낸 goto에 응답이 없으면 fleetd는 실행 중인 명령을 실패로 본다.
3. ID는 만료 검사 **전에** 기록한다. `expires_at`이 있고 로봇 벽시계가 그보다 늦으면 `failed`, 메시지 `"expired"`로 답하고 실행하지 않는다.
   같은 명령이 다시 오면 중복이므로 같은 `expired` ack가 다시 나간다(`adapter/fleet_logic.py:64-72`, `adapter/fleet_adapter.py:363-365`, `adapter/test_fleet_logic.py:61-67`).
   `expires_at`이 `null`이거나 없으면 만료되지 않는다.

### 4.3 ack (`fleet/<robot>/cmd_ack`, QoS 1)

```json
{"cmd_id":"t-2.0.1.oywypl","status":"cancelled","message":"cancel requested by t-2.0.1.oywypl.x1794989140517"}
```

`status`는 `accepted`, `done`, `failed`, `cancelled` 중 하나다(`internal/task/task.go:107-114`). `message`는 사람이 읽을 이유다.
ack를 보낼 때마다 CommandBook에 그 ack를 기록한다(중복에 다시 보낼 것, `adapter/fleet_adapter.py:123-128`). `floor`는 `ride`의 `done`에만 쓴다(ELEVATOR.md).

fleetd 쪽 처리(`internal/task/manager.go:300-347`):
- `dock.`으로 시작하는 ID의 ack는 로그만 남긴다(`internal/task/manager.go:303-305`, `internal/task/battery.go:245-255`).
- 그 밖에는 **작업의 현재 `cmd_id`이고 그 작업을 맡은 로봇이 보낸 것**만 본다. 나머지(늦은 ack, cancel의 ack 등)는 무시한다(`internal/task/manager.go:307-311`).
- `accepted` → RUNNING(`internal/task/manager.go:314-319`), `done` → 다음 구간 또는 DONE(`internal/task/manager.go:320-333`),
  `failed`/`cancelled` → 이번 시도 실패, 다시 대기열로(최대 시도 초과 시 FAILED, `internal/task/manager.go:334-343`, `internal/task/manager.go:517-532`).

### 4.4 `goto`

어댑터의 순서(`adapter/fleet_adapter.py:389-406`)와 목표 추적(`adapter/fleet_logic.py:100-171`):

1. `x`, `y`가 숫자가 아니면 `failed` `"goto needs numeric x and y"`(`adapter/fleet_adapter.py:390-395`).
2. 이미 몰고 있는 goto가 있으면 그것을 **`cancelled`** `"preempted by <새 cmd_id>"`로 끝낸다. `failed`가 아니다(`adapter/fleet_adapter.py:402-404`, `adapter/fleet_logic.py:119-123`).
3. 새 goto에 `accepted`를 보내고, 그 순간부터 state의 `task_id`를 이 `cmd_id`로 보낸다(`adapter/fleet_adapter.py:406`, `adapter/fleet_adapter.py:311`).
4. 끝날 때 **최종 ack 하나**를 보내고 `task_id`를 `""`로 되돌린다:
   - 목표에 도착해 멈춤 → `done` (`adapter/fleet_logic.py:167-169`; 도착 판정은 mock 0.05 m `mock_robots/mock_fleet.py:63`, `mock_robots/mock_fleet.py:334-339`)
   - 로봇이 ERROR → `failed` `"robot in ERROR: <error>"` (`adapter/fleet_logic.py:163-164`)
   - 다른 목표로 바뀜(새 goto, dock) 또는 도착 전에 멈춤 → `cancelled` (`adapter/fleet_logic.py:165-170`, `adapter/test_fleet_logic.py:145-156`)
   - 지금 위치가 곧 목표인 goto도 `accepted` 뒤 `done`이 나간다(`adapter/fleet_logic.py:152-155`, `adapter/test_fleet_logic.py:123-126`).
5. 어댑터는 로봇이 3 s 안에 목표를 받지 않으면 `failed` `"robot did not take the goal"`로 끝낸다(`adapter/fleet_logic.py:16`, `adapter/fleet_logic.py:158-159`).
   로봇이 스스로 말하는 경우에는 받을 수 없는 goto(ERROR 상태, 배터리 0)를 바로 `failed`로 답하면 된다.

fleetd 쪽: `task_id`가 그 `cmd_id`인 state도 `accepted`와 같은 확인으로 친다(`internal/task/manager.go:418-420`).
아무 확인도 없으면 3 s(AckTimeout)마다 같은 `cmd_id`로 최대 3번 다시 보내고, 그래도 없으면 그 로봇을 1분 쉬게 하고 작업을 다른 로봇에 넘긴다
(`internal/task/manager.go:54-55`, `internal/task/manager.go:61`, `internal/task/manager.go:422-436`).
구간에는 제한 시간이 있다: 거리/0.3 m/s + 30 s. 넘기면 cancel을 보내고 실패 처리한다(`internal/task/manager.go:62-63`, `internal/task/manager.go:408-417`).

### 4.5 `cancel` (`target` 포함)

fleetd가 cancel을 보내는 경우: 사용자가 작업을 취소(`internal/task/manager.go:274-296`), 구간 시간 초과(`internal/task/manager.go:413-416`),
배터리 위험 수준에서 작업 반납(`internal/task/battery.go:190-197`), 그리고 **빼앗긴 goto를 로봇이 계속 몰고 있을 때**(watch, `internal/task/manager.go:661-687`; 안전망 `internal/task/manager.go:693-710`).
마지막 경우 fleetd는 로봇의 `task_id`가 그 goto인 동안 3 s마다 cancel을 반복한다. 그래서 로봇은 cancel을 받으면 **반드시 멈추고 `task_id`를 비워야** 한다.

어댑터(ROS 로봇)의 동작(`adapter/fleet_adapter.py:380-384`, `adapter/fleet_adapter.py:407-423`, `mock_robots/mock_fleet.py:161-169`):
- `target`은 `ride`에만 쓰고, goto에는 무시한다. 로봇이 MOVING 또는 DOCKING이면 무엇이든 멈추고 IDLE이 된다.
- 멈췄으면 몰던 goto를 먼저 `cancelled` `"cancel requested by <cancel id>"`로 끝내고(`adapter/fleet_logic.py:125-132`), 그다음 cancel 자신에 `accepted`.
  이 순서 덕분에 로봇의 다음 state보다 cancel이 먼저 와도 goto가 `failed`("did not take the goal")가 아니라 `cancelled`로 끝난다(`adapter/test_fleet_logic.py:166-173`).
- 멈출 것이 없으면(IDLE, CHARGING, ERROR) cancel에 `failed` `"nothing to cancel (<mode>)"`.

Unity 로봇(`unity/FleetUnity/Assets/Fleet/Core/FleetRobot.cs`의 `DoCancel`)은 한 가지가 다르다(의도한 차이):
**`target`이 있고 지금 몰고 있는 goto가 아니면 아무것도 멈추지 않고 `failed`로 답한다.** fleetd는 cancel에 항상 대상 goto를 적어 보내므로(`internal/task/manager.go:741`),
늦게 도착한 cancel이 그사이 받은 새 작업을 멈추는 일을 막는다. `target`이 비어 있으면 어댑터와 같이 무엇이든 멈춘다.
fleetd는 cancel의 ack를 쓰지 않으므로(`internal/task/manager.go:307-311`) 이 차이는 fleetd 동작에 영향이 없다.

### 4.6 `dock`

배터리 정책이 보낸다(5.2절). 어댑터(`adapter/fleet_adapter.py:407-423`)와 mock(`mock_robots/mock_fleet.py:171-180`, `mock_robots/mock_fleet.py:306-309`, `mock_robots/mock_fleet.py:337`):
- ERROR면 `failed` `"in ERROR (<error>)"`.
- 아니면 몰던 goto를 `cancelled` `"dock requested by <dock id>"`로 끝내고, dock에 **`accepted` 하나만** 보낸다. dock에는 `done`이 없다.
- 로봇은 mode DOCKING으로 충전 위치까지 가고, 도착하면 CHARGING, 가득 차면(mock은 100 %) IDLE이 된다. dock은 `task_id`로 보고하지 않는다(`task_id`는 goto 전용).
- fleetd는 ack가 아니라 **mode**로 따라간다: dock을 보낸 뒤 10 s 안에 DOCKING/CHARGING이 안 보이면 다시 보내고, 3번 실패하면 1분 쉰다(`internal/task/battery.go:221-243`, `internal/task/manager.go:68-70`).

### 4.7 그 밖의 type

어댑터는 `reset_error`, `drop_comms`, `rescue`(서비스 호출)도 받는다(`adapter/fleet_adapter.py:67`, `adapter/fleet_adapter.py:407-413`). fleetd는 이것들을 보내지 않는다(운영·시험용).
모르는 type은 `failed` `"unknown command type '<type>'"`(`adapter/fleet_adapter.py:414-415`). 엘리베이터를 못 타는 로봇은 `ride`를 `failed`로 거절한다(8절).
Unity 로봇은 `reset_error`와 `rescue`(사람이 차에서 꺼냄, ELEVATOR.md 6장)를 받는다. `drop_comms`는 받지 않는다.

## 5. fleetd가 보는 것

### 5.1 연결 상태 (침묵과 호스트 offline)

판단은 250 ms마다 한다(`cmd/fleetd/main.go:145-153`). 기준은 fleetd가 마지막으로 **받아들인**(3.1절의 boot/seq를 통과한) pose 또는 state의 수신 시각이다(`internal/fleet/registry.go:181-183`).

| 상태 | 언제 | fleetd의 대응 | 근거 |
|---|---|---|---|
| ONLINE | 메시지가 정상적으로 옴 | 작업을 줄 수 있음 | `internal/fleet/registry.go:47` |
| STALE | 2 s 넘게 아무것도 안 옴 | 맡은 작업은 유지하고 시계를 멈춤(명령·ack가 오갈 수 없으므로). 새 작업은 안 줌 | `internal/fleet/registry.go:48`, `internal/fleet/registry.go:300-301`, `internal/task/manager.go:393-400`, `internal/task/manager.go:581` |
| OFFLINE | 20 s 넘게 아무것도 안 옴, **또는 그 `host`의 `offline` 상태 메시지** | 맡은 작업을 실패 처리하고 다른 로봇에 넘김. 그 goto는 watch로 계속 지켜보다가 로봇이 돌아와 몰고 있으면 cancel | `internal/fleet/registry.go:49`, `internal/fleet/registry.go:298-299`, `internal/fleet/registry.go:261-274`, `internal/task/manager.go:401-403`, `internal/task/manager.go:515-532` |

- STALE/OFFLINE에서 ONLINE으로 돌아가려면 받아들인 메시지가 **연속 3개** 필요하다. 그 사이 2 s 넘는 공백이 있으면 처음부터 다시 센다(`internal/fleet/registry.go:76-77`, `internal/fleet/registry.go:186-193`, `internal/fleet/registry.go:294-296`).
- 그래서 pose·state 하트비트(각 1 s)를 지키면 정지한 로봇도 ONLINE으로 남는다. 하트비트가 2 s를 넘기면 STALE이 된다.
- ERROR 모드인 로봇의 작업도 실패 처리된다(`internal/task/manager.go:404-406`).

### 5.2 배터리 정책

`internal/task/battery.go:11-36`, 기준값 `internal/task/manager.go:65-67`:
- 25 % 미만이면 "충전 필요": 새 작업을 받지 않고, IDLE이며 작업이 없을 때 `dock`을 받는다. 90 % 이상이 되어야 다시 작업을 받는다(`internal/task/battery.go:135-141`).
- 10 % 미만에서 작업을 몰고 있으면 cancel로 작업을 반납시킨다(시도 횟수는 쓰지 않음, `internal/task/battery.go:160-166`, `internal/task/battery.go:190-219`).
- CHARGING에서 90 % 전에 IDLE로 돌아오면 한 번은 다시 dock을 보낸다. 두 번째면 25 % 이상일 때 그대로 받아들인다(`internal/task/battery.go:172-188`).
  그러니 로봇의 "충전 완료"는 **90 % 이상**이어야 한다.
- **`battery`가 `null`이거나 없으면 그 로봇은 정책에서 빠진다**: 충전 필요로 표시되지 않고 dock도 받지 않으며 작업은 받을 수 있다.
  fleetd는 이것을 한 번 경고로 남긴다(`internal/task/battery.go:31-32`, `internal/task/battery.go:61-64`, `internal/task/battery.go:118-124`, `internal/fleet/registry.go:98-100`, `internal/fleet/registry.go:239-242`).

### 5.3 작업을 받을 수 있는 로봇

`internal/task/manager.go:579-600`: ONLINE, mode `IDLE`, `error` 없음, pose를 받았음, 충전 필요 아님, 보낸 dock이 아직 도착할 수 있는 시간이 아님,
`task_id`가 빔, 다른 작업을 쥐고 있지 않음, 쉬는 중이 아님. 그중 목표에 가장 가까운 로봇(층이 다르면 층마다 1000 m를 더함, `internal/task/manager.go:53`, `internal/task/manager.go:622-640`)이 고른다.

## 6. 모드

| mode | 뜻 | 근거 |
|---|---|---|
| `IDLE` | 서 있음. 작업을 받을 수 있는 유일한 모드 | `mock_robots/mock_fleet.py:60`, `internal/task/manager.go:583` |
| `MOVING` | goto를 몰고 있음 | `mock_robots/mock_fleet.py:158` |
| `DOCKING` | 충전 위치로 가는 중 | `mock_robots/mock_fleet.py:178` |
| `CHARGING` | 충전 중. 가득 차면 IDLE | `mock_robots/mock_fleet.py:306-309` |
| `ERROR` | 고장. `error`에 이유. 작업은 실패 처리되고 dock도 거절 | `mock_robots/mock_fleet.py:346-351`, `internal/task/manager.go:404-406` |
| `WAITING_ELEVATOR`, `BOARDING`, `RIDING`, `EXITING` | 엘리베이터 탑승 중(어댑터가 덮어씀). 타지 않는 로봇은 쓰지 않음 | `adapter/fleet_logic.py:286`, ELEVATOR.md |

fleetd는 mode가 바뀔 때마다 `mode` 이벤트를, error가 바뀔 때마다 `error` 이벤트를 남긴다(`internal/fleet/registry.go:228-233`).

## 7. 정리: 직접 말하는 로봇의 할 일

1. 프로세스 시작: 새 `boot`. MQTT 연결 하나, LWT `offline`(retained). 연결될 때마다 `online`(retained) + 로봇마다 `fleet/<robot>/cmd` 구독(QoS 1).
2. 로봇마다: pose(QoS 0, ≤5 Hz, 5 cm/2° 데드밴드, 1 s 하트비트), state(QoS 1, 바뀌면 즉시, 1 s 하트비트). 둘 다 `boot`, 토픽별 `seq`, 벽시계 `stamp`, 같은 `host`, pose에는 `floor`.
3. 명령: 중복이면 마지막 ack 반복 → 만료면 `failed`/`expired` → type별 처리(4.4~4.7절). goto의 끝은 `done`/`failed`/`cancelled` 중 하나, 앞선 goto가 밀려나면 `cancelled`.
4. `task_id` = 몰고 있는 goto의 `cmd_id`, 그 밖에는 `""`.
5. 배터리를 모르면 `null`. 알면 0..100으로 잘라 보낸다.
6. 엘리베이터를 타면 ELEVATOR.md 5장 전부(`ride`, `elevator/<id>/turn|state|trip` 구독, `zone`, 끊김, `event`). Unity 로봇의 구현은 9.1절.
   못 타면 모든 state에 `"can_ride": false`.
7. 정상 종료: `offline`(retained)을 보내고 끊는다.

## 8. `can_ride` (선택, 엘리베이터를 못 타는 로봇)

이 필드는 이 문서를 쓰는 시점에 다른 작업으로 fleetd에 들어가는 중이었다(작업 트리에 커밋 전 상태로 있음). 그 코드 기준:

- state의 `can_ride`(bool)는 선택이다. **빠지면 true**다(`internal/fleet/registry.go:40-42`). 매 state마다 다시 읽으므로, false를 보내다가 필드를 빼면 다시 true가 된다(`internal/fleet/registry.go:238`).
  pose만 받은 로봇도 true로 시작한다(`internal/fleet/registry.go:149`). `GET /robots`에 `can_ride`로 보인다(`internal/fleet/registry.go:101-103`).
- `can_ride: false`인 로봇은 **남은 구간 중 하나라도 자기 층이 아닌 작업을 받지 않는다**(`internal/task/manager.go:603-617`의 `mustRide`, `internal/task/manager.go:622-628`의 `pick`).
  그런 작업은 탈 수 있는 로봇이 빌 때까지 대기열에 남는다(`internal/task/manager.go:562-564`). 엘리베이터가 하나도 설정되지 않았으면 이 제한은 없다(층이 다른 작업은 어차피 실패한다).
- 배정 뒤에 false로 바뀐 경우도 막는다. 다른 층 구간을 준비할 때 그 로봇이 못 탄다고 하면, 작업 전체가 아니라 **그 로봇의 시도 하나만** 실패시키고 다른 로봇에 넘긴다
  (`internal/task/ride.go:69-72`, `internal/task/ride.go:84-95`).
- 그래서 못 타는 로봇은 `ride` 명령을 받지 않는다. 그래도 오면 `failed`로 답한다(4.7절).

Unity 로봇은 기본으로 모든 state에 `"can_ride": true`를 보내고 엘리베이터를 탄다(9.1절).
`-canRide false`(`unity_robots.ps1 -CanRide false`)로 띄우면 예전처럼 모든 state에 `"can_ride": false`를 보내고, `ride`는 `failed` `"this robot cannot ride elevators (can_ride false)"`로 거절하며
엘리베이터 채널을 구독하지 않는다.

## 9. Unity 로봇 (`unity/FleetUnity`)

ROS 없이 이 문서대로 MQTT를 직접 말하는 시뮬레이터다. 한 Unity 프로세스가 MQTT 연결 하나(호스트 `-host`, 기본 `unity-1`)로 로봇 N대를 돌린다.

| 경로 | 내용 |
|---|---|
| `Assets/Fleet/Core` | 프로토콜 핵심. 순수 C#, UnityEngine 참조 없음(asmdef `noEngineReferences`). `fleet_logic.py`를 옮긴 것: `CommandBook`, 발행 정책(`Throttle`), 로봇 1대의 명령·ack·발행(`FleetRobot`), 엘리베이터 탑승(`RobotRide`, 9.1절), 운동·배터리·엘리베이터 움직임(`RobotSim`, mock_fleet.py와 같은 값), 명령줄(`BootArgs`) |
| `Assets/Fleet/Unity` | `FleetMqtt`(MQTTnet 4.3.7, 연결·LWT·재연결·inbox·송신 큐 하나, `elevator/+/turn|state|trip` 구독), `FleetBootstrap`(로봇 생성, 매 프레임 core 구동, 엘리베이터 메시지를 그 차를 탄 로봇에게 전달, 시작 시 자동 생성), `RobotView`(원기둥 + 방향 표시, 바닥·카메라) |
| `Assets/Fleet/Editor/BuildScript.cs` | `-executeMethod Fleet.EditorTools.BuildScript.BuildWindows` → `Build/FleetRobots.exe` (Mono, 스트리핑 끔) |
| `Assets/Fleet/Tests` | EditMode 테스트(`adapter/test_fleet_logic.py`의 해당 사례 + ack 수명·cancel·dock·배터리, `adapter/test_ride_logic.py` 34개 사례를 옮긴 `RobotRideTests`, 로봇별 끊김·명령 버림·와이파이 복귀 순서·하차 실패·갇힘·`can_ride false`의 `FleetRobotRideTests`) |
| `Assets/Plugins/MQTTnet/MQTTnet.dll` | nuget.org의 MQTTnet 4.3.7.1207 패키지 `lib/netstandard2.1` 그대로(MIT) |

- 좌표: 지도 (x, y) = Unity (x, z). 위에서 보면 x는 오른쪽, y는 위. yaw는 +x에서 반시계.
- 시계: 발행 정책은 단조 시계(`Stopwatch`), `stamp`와 `expires_at` 비교는 벽시계 유닉스 초. `Time.time`은 쓰지 않는다.
- 배터리: 움직이면 0.05 %/m, 켜져 있으면 0.002 %/s 줄고, 충전 중 1 %/s 늘며 0..100으로 자른다. dock 위치는 시작 위치다. 이동 중 0 %가 되면 ERROR `battery_empty`.
- 연결이 끊긴 동안 나갈 메시지는 버린다. fleetd가 명령을 다시 보내고 로봇은 매초 pose·state를 다시 보내므로 따로 쌓아 두지 않는다. 다시 연결되면 pose·state를 바로 보낸다.

실행:

```powershell
powershell -ExecutionPolicy Bypass -File scripts\unity_build.ps1 -Test     # 테스트 + 빌드
powershell -ExecutionPolicy Bypass -File scripts\unity_robots.ps1 -Robots 2 -HostId unity-1 -Floor 1
# 플레이어 직접: Build\FleetRobots.exe -batchmode -nographics -broker 127.0.0.1:1883 -host unity-1
#               -robots 2 -prefix unity -floor 1 -origin 0,-3 -spacing 2 -speed 0.8 [-battery 100]
```

로봇 이름은 `<prefix>_1..N`이다. 플레이어를 여러 개 띄울 때는 `-host`와 `-prefix`를 모두 다르게 준다.

### 9.1 엘리베이터 탑승

Unity 로봇은 [ELEVATOR.md](ELEVATOR.md) 5장을 ROS mock 로봇 + 어댑터와 **똑같이** 따른다. 엘리베이터 규약은 바뀌지 않았다. fleetd·mock 엘리베이터는 두 종류를 구분하지 않으므로
ROS 로봇과 Unity 로봇이 한 운행(한 차)에 섞여 탄다. 탑승 순서·칸·`turn`·`zone`·`door_zone` 규칙도 같다.

| 무엇 | Unity 쪽 | 원본 |
|---|---|---|
| 탑승 논리 | `Core/RobotRide.cs`: `RobotRide`를 동작 그대로 옮김(수락·검증·`WAITING_ELEVATOR`, `n` 중복과 답 다시 보내기, 움직이는 중 무응답, `BOARDING`+`silence_s`, 끊김, 와이파이 복귀 순서, 탑승 실패 → 로비, 하차 실패 → 자기 칸, 움직임 시간 제한, 로봇을 태운 채 끝난 운행 → ERROR "needs a human", 탑승 전 취소, 이벤트 seq). 메서드마다 원본 줄을 주석으로 단다 | `adapter/fleet_logic.py:174-546` |
| 배선 | `Core/FleetRobot.cs`: 끊김 동안 `fleet/*` 발행 없음(예외: 탑승 직전 `BOARDING` 상태와 이벤트 액션), 받은 명령은 **파싱·중복 검사 전에** 버림, ack는 기록만, 복귀 때 pose·state를 정책 무시하고 바로 보냄, `ride` 수락이 진행 중 goto를 `cancelled`로 끝냄, ride를 든 동안 goto·dock 거절 | `adapter/fleet_adapter.py`의 `handle_cmd`, `run`, `send_fleet`, `tick_rides`, `publish_state/pose` |
| 움직임 | `Core/RobotSim.cs`의 `StartMove`: `board`(로비 → 문 → 칸), `floor`(차 안일 때만 층 바꿈), `exit`(시작할 때 층을 F로, 칸 → 문 → 그 turn의 로비). 차 안 위치는 로비에서 +x로 문 +1.0 m, 칸 0 +1.8 m, 칸마다 +0.8 m. `in_car`는 움직임이 끝날 때만 바뀐다. 움직임 중 고장은 그 자리에 멈추고 `running`으로 남다가 `reset_error`가 시작점으로 되돌린 뒤 `failed` | `mock_robots/mock_fleet.py`의 `on_elevator_move`, `drive_move`, `on_reset_error`, `on_rescue` |
| 문 구역 시각 | `zone enter`는 움직임을 시작하기 전 프레임에 나가고, `clear`는 칸(탈 때)이나 로비(내릴 때)에 닿은 뒤에 나간다 | ELEVATOR.md 3.10 |
| MQTT | 연결 하나가 `fleet/<robot>/cmd`(로봇마다) + `elevator/+/turn`, `elevator/+/state`, `elevator/+/trip`(와일드카드, 재연결 때 다시)을 구독한다. 엘리베이터 메시지는 한 번 파싱해 그 차를 탄 로봇에게만 준다. 빈 retained `trip`은 "운행 끝"(지워짐), 객체가 아닌 본문은 무시. retained `trip`의 `turn`으로는 움직이지 않는다. 끊김은 로봇별이라 같은 프로세스의 다른 로봇은 계속 말한다 | `adapter/fleet_adapter.py`의 `subscribe_car`, `handle_elevator` |

어댑터와 다른 점(규약에는 영향 없음): 로봇과 "어댑터"가 한 프로세스라 로봇은 항상 살아 있는 것으로 보고(어댑터의 `is_live`), 움직임 결과를 2 Hz `robot_state`가 아니라 매 프레임 읽는다.
움직임 id는 실행마다 무작위 값에서 시작한다(어댑터의 `move_base`와 같다).

실패 주입(시험용, mock 로봇·어댑터 플래그와 같은 뜻): `-exitFailFloor F [-exitFailCount N]`(F층 하차가 문 구역에서 막혀 칸으로 돌아감, 로봇마다 N번, -1은 매번),
`-doorMoveS s`(탑승·하차가 최소 s초), `-ignoreTurns robot:n[,robot:n]`(자기 turn n개를 잃어버림), `-moveTimeoutExtra s`(기본 30).
`unity_robots.ps1`에는 `-CanRide`, `-ExitFailFloor`, `-ExitFailCount`, `-DoorMoveS`, `-IgnoreTurns`로 있다.

검증(2026-11-19, Mosquitto + fleetd + mock 엘리베이터): Unity 2대만 3·5층 / ROS 2대 + Unity 2대가 2·3·4·5층(한 운행에 `unity_1`·`robot_1`·`robot_2`) /
Unity 하차 1번 막힘(같은 층 재시도로 내림)·2번 막힘(5층으로 넘겨져 내린 뒤 다시 3층으로) 모두 작업 DONE, `door_zone` 최대 1, 차 안 로봇의 `fleet/*` 0건,
복귀 순서 pose → state → 이벤트 → `done`(+floor). `scripts/mixed_fleet_check.py`(기본: Unity도 다른 층 작업을 받음, `--unity-no-ride`: `-canRide false`로 띄운 Unity).
