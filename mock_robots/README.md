# Mock robots (ROS 2, Gazebo 없이)

관제 서버 개발용 가짜 로봇입니다. 맵, 장애물, 센서 없이 **관제 서버가 보는 것만** 흉내 냅니다.
위치, 배터리, 상태, 목표 이동, 도킹·충전, 고장, 통신 끊김이 여기에 해당합니다.
이동은 목표까지 직선으로 갑니다.

## 실행 (Windows, 명령 프롬프트 cmd)

```bat
cd C:\pixi_ws
pixi shell
call C:\pixi_ws\ros2-windows\local_setup.bat
python <이 폴더>\mock_fleet.py --count 3
```

| 옵션 | 기본값 | 설명 |
|---|---|---|
| `--count` | 3 | 로봇 수 (`robot_1`, `robot_2`, ...) |
| `--speed` | 0.8 | 이동 속도 m/s |
| `--spacing` | 2.0 | 도크 간격 m. 로봇 i는 (시작 x + i×간격, 0)에서 시작, 거기가 도크 |
| `--origin-x` | 0 | 첫 도크의 x (다른 mock 프로세스와 떨어뜨려 놓을 때) |
| `--fault-rate` | 0 | 로봇당 분당 무작위 고장 횟수 |
| `--drop-seconds` | 10 | `drop_comms` 호출 시 침묵하는 시간 |
| `--roam-rate`, `--roam-max` | 0, 4 | 로밍 끊김: 로봇당 분당 횟수, 가장 긴 끊김(초) |
| `--battery` | 없음 | 모든 로봇의 시작 배터리(%). 없으면 60~100% 무작위 |
| `--drain-per-m` | 0.05 | 이동 1 m당 배터리 감소(%) |
| `--charge-rate` | 1.0 | 도크에서 초당 충전(%) |
| `--seed` | 없음 | 시작 배터리 난수 고정 |

## 로봇당 인터페이스 (네임스페이스 = 로봇 이름)

| 종류 | 이름 | 타입 | 비고 |
|---|---|---|---|
| pub | `odom` | nav_msgs/Odometry | 10 Hz, best effort (센서 QoS) |
| pub | `battery_state` | sensor_msgs/BatteryState | 1 Hz |
| pub | `robot_state` | std_msgs/String (JSON) | 2 Hz, reliable + transient local (늦게 붙어도 마지막 상태 받음) |
| sub | `goal_pose` | geometry_msgs/PoseStamped | 목표 지점 |
| srv | `cancel` | std_srvs/Trigger | 이동 취소 |
| srv | `dock` | std_srvs/Trigger | 도크로 가서 충전, 100%가 되면 IDLE |
| srv | `reset_error` | std_srvs/Trigger | ERROR 해제 |
| srv | `drop_comms` | std_srvs/Trigger | 일정 시간 모든 발행 중단 (heartbeat 감지 테스트) |

TF `map → <name>/base_link`도 발행합니다(RViz 확인용).

상태(`mode`): `IDLE`, `MOVING`, `DOCKING`, `CHARGING`, `ERROR`

`robot_state` 예시:
```json
{"name": "robot_1", "mode": "MOVING", "x": 1.2, "y": 0.4, "yaw": 0.32, "frame": "map",
 "battery": 87.3, "goal": [3.0, 1.0, 0.0], "error": "", "stamp": 1793500000.12}
```

## 손으로 테스트하기 (다른 cmd 창, 같은 환경 설정 후)

```bat
ros2 topic list
ros2 topic echo /robot_1/robot_state
ros2 topic pub --once /robot_1/goal_pose geometry_msgs/msg/PoseStamped "{header: {frame_id: map}, pose: {position: {x: 3.0, y: 1.0}, orientation: {w: 1.0}}}"
ros2 service call /robot_1/dock std_srvs/srv/Trigger
ros2 service call /robot_1/cancel std_srvs/srv/Trigger
ros2 service call /robot_2/drop_comms std_srvs/srv/Trigger
ros2 service call /robot_2/reset_error std_srvs/srv/Trigger
```

## 시뮬레이션 규칙

- 배터리: 켜져 있으면 초당 0.002%, 이동하면 m당 0.05%(`--drain-per-m`) 감소. 도크에서 초당 1%(`--charge-rate`) 충전.
- `dock`: 하던 이동을 멈추고 `DOCKING`으로 자기 도크(시작 위치)까지 간 뒤 `CHARGING`, 100%가 되면 `IDLE`.
- 이동 중 배터리 0% → `ERROR (battery_empty)`. ERROR 중에는 목표를 무시합니다.
- `--fault-rate`를 주면 무작위로 `ERROR (simulated_fault)`가 납니다.
- 배터리 부족 시 충전 보내기(서버의 배터리 정책, 루트 README), 작업 배정, 통신 끊김 판정은 **관제 서버가 할 일**이라 로봇 쪽에는 넣지 않았습니다.

## 나중에 실제 Nav2로 바꿀 때

`goal_pose`(PoseStamped)와 `odom`은 Nav2와 같은 타입입니다. 관제 서버가 이 두 개와 `robot_state`만 쓰도록 만들어 두면,
WSL/Ubuntu에서 Nav2 로봇을 띄울 때 `robot_state`를 내는 작은 어댑터 노드만 추가하면 됩니다.
