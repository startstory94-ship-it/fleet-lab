# fleet-lab

ROS 2 mock 로봇과 Go 관제 서버(fleetd)로 만드는 로봇 플릿 관제 실험실.
핵심 기능은 목적층이 다른 로봇을 엘리베이터에 함께 태우는 것이다. 설계와 단계는 [PLAN.md](PLAN.md)에 있다.

## 폴더

| 폴더 | 내용 |
|---|---|
| `cmd/fleetd` | 관제 서버 진입점 |
| `internal/fleet` | 로봇 목록, seq 검사, 연결 상태(ONLINE/STALE/OFFLINE/IN_ELEVATOR), MQTT 메시지 수신 |
| `internal/events` | 이벤트 배포 (느린 수신자는 건너뜀), 최근 이벤트 보관 |
| `internal/mqttx` | MQTT 연결 (재접속 백오프 + jitter, 재구독) |
| `internal/task` | 작업 배정·명령 재전송·재배정·취소 (가짜 시계로 테스트) |
| `internal/store` | 작업을 SQLite에 저장 (modernc.org/sqlite, cgo 없음) |
| `internal/sim` | 통합 테스트용 가짜 로봇 (MQTT·ROS 없이 실제 Registry와 Manager에 연결) |
| `internal/api` | REST API |
| `mock_robots/` | ROS 2 mock 로봇 (Gazebo 없음) |
| `adapter/` | ROS 2 ↔ MQTT 어댑터 |
| `docs/` | 테스트 계획과 리뷰 기록 |
| `scripts/` | 끝단 시나리오 테스트 (`e2e.bat`) |

## 필요한 것

- Go 1.27+
- Mosquitto (localhost:1883)
- ROS 2 Jazzy + Python `paho-mqtt` (mock 로봇과 어댑터용)

## 실행

창 세 개를 쓴다. 로봇과 어댑터 창은 먼저 ROS 2 환경을 켠다.

```bat
cd C:\pixi_ws
pixi shell
call C:\pixi_ws\ros2-windows\local_setup.bat
```

1. 관제 서버: `go run ./cmd/fleetd` (`-log debug`를 붙이면 버려진 오래된 메시지도 보인다)
2. mock 로봇: `python mock_robots\mock_fleet.py --count 3`
3. 어댑터: `python adapter\fleet_adapter.py`

확인:

```bat
curl http://127.0.0.1:8080/robots
mosquitto_sub -t "fleet/#" -v
```

명령 보내기 (`cmd_id`는 매번 달라야 한다. 같은 ID는 중복으로 보고 무시한다):

```bat
mosquitto_pub -t fleet/robot_1/cmd -q 1 -m "{\"cmd_id\":\"c1\",\"type\":\"goto\",\"x\":3,\"y\":2}"
```

명령 종류: `goto`(x, y, yaw), `cancel`, `dock`, `reset_error`, `drop_comms`. 결과는 `fleet/<robot>/cmd_ack`로 온다.
`goto`는 `accepted` 뒤에 `done` / `failed` / `cancelled`가 한 번 더 온다. 수행 중에는 로봇 상태의 `task_id`가 그 `cmd_id`다.
같은 `cmd_id`를 다시 보내면 실행하지 않고 마지막 응답을 다시 보낸다.

## 테스트

```bat
go test ./...
cd C:\pixi_ws && pixi run python -m pytest C:\Users\han\Documents\fleet-lab\adapter
scripts\e2e.bat
```

`e2e.bat`은 서버·mock 로봇·어댑터를 실제로 띄워 시나리오 24개를 돌린다(약 2분, Mosquitto 필요).
작업 배정, 서버 재시작 후 작업 이어가기도 여기서 확인한다.
테스트 목록과 리뷰는 [docs/TEST_PLAN.md](docs/TEST_PLAN.md).

## API

| 경로 | 내용 |
|---|---|
| `GET /robots`, `GET /robots/{name}` | 로봇 상태 (`conn` 포함) |
| `GET /events?n=50` | 최근 이벤트 (최대 200개) |
| `GET /ws` | WebSocket 실시간 이벤트 |
| `POST /tasks` | 작업 추가. 본문 `{"waypoints":[{"x":1,"y":2,"floor":1}]}` (x, y, floor 모두 필수, 뒤에 다른 데이터 없음) → `201` + 작업. 형식이 틀리면 `400`, 1 MB 넘으면 `413`, 저장 실패면 `500`(작업은 만들어지지 않음) |
| `GET /tasks?state=QUEUED` | 작업 목록 (만든 순서). `state`는 선택, 모르는 값이면 `400` |
| `GET /tasks/{id}` | 작업 하나. 없으면 `404` |
| `POST /tasks/{id}/cancel` | 취소. 로봇이 하던 중이면 `cancel` 명령을 보낸다. 이미 끝난 작업은 `409`, 없으면 `404` |

## 연결 상태

| 상태 | 조건 |
|---|---|
| `ONLINE` | 정상 수신 |
| `STALE` | 2초 이상 무응답 (와이파이 로밍 등). 하던 작업은 유지 |
| `OFFLINE` | 20초 이상 무응답, 또는 어댑터 LWT |
| `IN_ELEVATOR` | 로봇이 `silence_s`로 끊김을 예고함. 그 시간이 지나면 `elevator_overdue` 경보 |

복구는 연속 3번 정상 수신 후에 한다. 로밍 끊김은 `mock_fleet.py --roam-rate 6 --roam-max 4`로 흉내 낸다.

## 작업

작업은 지점 목록이다. 지점마다 한 구간(leg)씩 `goto`를 보내고, 마지막 지점에서 `done`이 오면 끝난다.

| 상태 | 뜻 |
|---|---|
| `QUEUED` | 로봇을 기다림 |
| `ASSIGNED` | 이번 구간의 `goto`를 보냄, 아직 `accepted` 전 |
| `RUNNING` | 로봇이 이번 구간을 받음 (`accepted` 또는 로봇 상태의 `task_id`가 그 `cmd_id`) |
| `DONE` / `FAILED` / `CANCELLED` | 끝. 목록에 계속 남는다 |

- **배정:** 먼저 들어온 작업부터. ONLINE·IDLE·에러 없음·배터리 기준 이상(지금은 0%, 4단계에서 올림)·`task_id` 없음·
  다른 작업이 없는 로봇 중 다음 지점에 가장 가까운 로봇. 다른 층은 층마다 1000 m를 더한다. 거리가 같으면 이름순.
  그 작업에 실패한 적이 있는 로봇(`failed_on`)은 다른 로봇이 비어 있으면 건너뛴다.
- **명령 ID:** `<작업>.<구간>.<배정 횟수>.<서버 실행 ID>` (예: `t-3.0.1.k3x9ab`). 서버 실행 ID는 시작할 때마다 새로 정하므로
  마지막 저장이 유실된 채 재시작해도 ID가 겹치지 않는다(겹치면 어댑터가 새 명령을 중복으로 보고 옛 응답을 돌려준다).
  `expires_at`은 보낼 때마다 지금 + 10초.
- **응답이 없으면:** 3초마다 같은 `cmd_id`로 다시 보낸다(최대 3번). 어댑터는 같은 ID를 다시 실행하지 않고 마지막 응답을 다시 보내므로,
  `done` 응답이 유실돼도 이렇게 되찾는다. 로봇이 STALE·엘리베이터 안이면 기다리는 시간을 세지 않는다.
  끝까지 답이 없는 로봇은 1분 동안 새 작업을 받지 않는다.
- **구간 시간 제한:** 거리 / 0.3 m/s + 30초. 넘으면 로봇에 `cancel`을 보내고 다시 대기열로. STALE·엘리베이터 시간은 세지 않는다.
- **실패:** `failed`/`cancelled` 응답(어댑터 시계가 빨라 `expired`가 와도 같다), 응답 없음, 시간 초과, 로봇 OFFLINE·ERROR면
  시도 횟수를 늘리고 다시 대기열로. 3번 실패 뒤 한 번 더 실패하면 `FAILED`.
- **두 로봇이 같은 작업을 하지 않게:**
  - 로봇에게서 작업을 거둘 때 (로봇, `cmd_id`)를 지켜본다. 그 로봇이 그 `cmd_id`를 수행 중이라고 보고하면 `cancel`을 보내고(3초마다 반복),
    보고하지 않아도 마지막으로 보낸 명령이 아직 도착할 수 있는 동안(보낸 시각 + 10초 + 5초)은 계속 지켜본다.
    위치만 오고 상태가 아직 없는 로봇은 "모름"으로 본다.
  - 안전망: ONLINE 로봇이 자기 것이 아닌 작업의 `cmd_id`를 3초 넘게 보고하면 지켜보기에 넣어 취소한다.
    지켜보기 목록은 메모리에만 있으므로 재시작 뒤에는 이 규칙이 대신한다.
- **저장:** 바뀐 작업은 별도 고루틴이 SQLite(`-db`)에 쓴다(작업마다 최신 상태만, 실패하면 1초마다 다시 시도).
  관리자 잠금을 잡은 채 디스크에 쓰지 않으므로 MQTT 수신이 멈추지 않는다. 메모리 상태가 기준이고, 갑자기 꺼지면 아직 쓰지 못한
  변경만 잃는다(아래 재시작 규칙이 복구한다). 새 작업만은 저장한 뒤에 만든다. 읽을 수 없는 행은 기록하고 건너뛴다.
- **재시작:** `QUEUED`는 그대로. `ASSIGNED`/`RUNNING`은 MQTT에 처음 연결된 뒤 25초(OFFLINE 기준 20초보다 길게) 안에 로봇이
  그 `cmd_id`를 보고하면 이어 가고, 아니면 대기열로 돌아간다. 그 사이 로봇이 다른 것을 보고하면 같은 `cmd_id`를 한 번 다시 보내 본다
  (서버가 꺼진 동안 끝났다면 어댑터가 `done`을 다시 보낸다). STALE·엘리베이터 안 로봇은 OFFLINE이 될 때까지 기다린다.
- **이벤트:** `/events`, `/ws`에 `type: "task"`로 `assigned`, `running`, `leg_done`, `done`, `requeued`, `failed`, `cancelled`가 나온다.
  로봇 이벤트와 같은 `seq` 번호를 쓴다.

```bat
curl -X POST http://127.0.0.1:8080/tasks -H "Content-Type: application/json" -d "{\"waypoints\":[{\"x\":3,\"y\":2,\"floor\":1}]}"
curl http://127.0.0.1:8080/tasks?state=RUNNING
curl -X POST http://127.0.0.1:8080/tasks/t-1/cancel
```
