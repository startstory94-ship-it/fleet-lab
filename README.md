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
| `internal/api` | REST API |
| `mock_robots/` | ROS 2 mock 로봇 (Gazebo 없음) |
| `adapter/` | ROS 2 ↔ MQTT 어댑터 |
| `docs/` | 설계 대화 기록 |

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

## API

| 경로 | 내용 |
|---|---|
| `GET /robots`, `GET /robots/{name}` | 로봇 상태 (`conn` 포함) |
| `GET /events?n=50` | 최근 이벤트 (최대 200개) |
| `GET /ws` | WebSocket 실시간 이벤트 |

## 연결 상태

| 상태 | 조건 |
|---|---|
| `ONLINE` | 정상 수신 |
| `STALE` | 2초 이상 무응답 (와이파이 로밍 등). 하던 작업은 유지 |
| `OFFLINE` | 20초 이상 무응답, 또는 어댑터 LWT |
| `IN_ELEVATOR` | 로봇이 `silence_s`로 끊김을 예고함. 그 시간이 지나면 `elevator_overdue` 경보 |

복구는 연속 3번 정상 수신 후에 한다. 로밍 끊김은 `mock_fleet.py --roam-rate 6 --roam-max 4`로 흉내 낸다.
