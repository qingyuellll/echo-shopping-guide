@echo off
chcp 65001 >nul
setlocal
cd /d "%~dp0"
if exist "guide.env.cmd" call "guide.env.cmd"
if not defined GUIDE_ADDR set "GUIDE_ADDR=127.0.0.1:8082"
if not defined TICKETING_API_BASE set "TICKETING_API_BASE=http://127.0.0.1:8081"
if not defined ECHO_WEB_BASE set "ECHO_WEB_BASE=http://127.0.0.1:3002"
echo Echo 回响 - 演出导购
echo 访问地址：http://%GUIDE_ADDR%
echo 请保持 Echo 的票务服务正常运行。
if not exist "backend\guide.exe" (
  echo 未找到导购程序，请先运行 build-guide.cmd。
  pause
  exit /b 1
)
if not exist "frontend\dist\index.html" (
  echo 未找到页面文件，请先运行 build-guide.cmd。
  pause
  exit /b 1
)
start "" "http://%GUIDE_ADDR%"
cd backend
guide.exe
pause
