package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/av/avutil"
	"github.com/nareix/joy4/format"
	"github.com/nareix/joy4/format/rtmp"
)

const (
	maxRetries            = 3                      // Число максимальных попыток
	retryDelay            = 5                      // Секунд между попытками
	statusPackets         = 1000                   // После скольких пакетов показывать статус
	minTimeBetweenPackets = 1 * time.Millisecond   // Минимальное время между пакетами
	maxJitterCorrection   = 50 * time.Millisecond  // Максимальная коррекция джиттера
	initialBufferSize     = 100                    // Размер начального буфера пакетов
	packetQueueSize       = 100                    // Размер очереди для асинхронной отправки
	audioSyncThreshold    = 100 * time.Millisecond // Порог для синхронизации аудио
	preloadNextFileTime   = 5 * time.Second        // Время до конца файла для начала подготовки следующего
	minBitrate            = 1500000                // Минимальный битрейт (1.5 Mbps)
	reconnectTimeout      = 10 * time.Second       // Таймаут для переподключения к RTMP
	maxConsecutiveErrors  = 10                     // Максимальное количество ошибок подряд перед перезапуском соединения
	minPlayTime           = 60 * time.Second       // Минимальное время воспроизведения каждого файла
	stateFilePath         = "stream_state.json"    // Путь к файлу состояния потока
	saveStateInterval     = 30 * time.Second       // Интервал сохранения состояния
)

// Config структура для загрузки конфигурации
type Config struct {
	RTMP struct {
		URL string `json:"url"`
		Key string `json:"key"`
	} `json:"rtmp"`
	Video struct {
		Directory string `json:"directory"`
		LoopMode  bool   `json:"loopMode"`
	} `json:"video"`
	Settings struct {
		ForceBitrate       int  `json:"forceBitrate"`       // Принудительно установить битрейт (бит/с), 0 = автоматически
		ForceKeyframe      bool `json:"forceKeyframe"`      // Принудительно генерировать ключевые кадры
		KeyframeSeconds    int  `json:"keyframeSeconds"`    // Интервал ключевых кадров в секундах
		ReconnectOnNewFile bool `json:"reconnectOnNewFile"` // Переподключаться при каждом новом файле
		DisableEarlyEnd    bool `json:"disableEarlyEnd"`    // Отключить раннее завершение файла
		MinPlayTime        int  `json:"minPlayTime"`        // Минимальное время воспроизведения каждого файла в секундах
		RestoreState       bool `json:"restoreState"`       // Восстанавливать состояние при запуске
	} `json:"settings"`
}

// StreamStatus содержит статус потоковой передачи
type StreamStatus struct {
	EndOfFile     bool          // Флаг окончания файла
	PrepareNext   bool          // Флаг необходимости подготовки следующего файла
	TotalPackets  int           // Общее количество отправленных пакетов
	VideoDuration time.Duration // Общая длительность видео
	ElapsedTime   time.Duration // Прошедшее время
	Bitrate       int64         // Оценка битрейта (бит/с)
}

// StreamState содержит информацию о состоянии стрима для сохранения/восстановления
type StreamState struct {
	CurrentFile  string        `json:"currentFile"`  // Текущий проигрываемый файл
	Position     time.Duration `json:"position"`     // Примерная позиция в файле
	LastSaveTime time.Time     `json:"lastSaveTime"` // Время последнего сохранения
	FileIndex    int           `json:"fileIndex"`    // Индекс файла в списке
}

// BitrateCalculator помогает отслеживать и вычислять битрейт
type BitrateCalculator struct {
	StartTime       time.Time // Время начала отсчета
	BytesSent       int64     // Количество отправленных байт
	SampleWindow    []int64   // Окно выборки для расчета скользящего среднего
	WindowSize      int       // Размер окна
	CurrentBitrate  int64     // Текущий битрейт в бит/с
	WindowStartTime time.Time // Время начала текущего окна
	WindowBytes     int64     // Байты в текущем окне
}

// NewBitrateCalculator создает новый калькулятор битрейта
func NewBitrateCalculator(windowSize int) *BitrateCalculator {
	return &BitrateCalculator{
		StartTime:       time.Now(),
		BytesSent:       0,
		SampleWindow:    make([]int64, 0, windowSize),
		WindowSize:      windowSize,
		CurrentBitrate:  0,
		WindowStartTime: time.Now(),
		WindowBytes:     0,
	}
}

// AddBytes добавляет байты и обновляет битрейт
func (bc *BitrateCalculator) AddBytes(bytes int64) {
	bc.BytesSent += bytes
	bc.WindowBytes += bytes

	// Обновляем средний битрейт, если прошло достаточно времени
	elapsed := time.Since(bc.WindowStartTime)
	if elapsed >= time.Second {
		bytesPerSecond := float64(bc.WindowBytes) / elapsed.Seconds()
		bitrate := int64(bytesPerSecond * 8) // переводим в биты в секунду

		// Добавляем в окно
		bc.SampleWindow = append(bc.SampleWindow, bitrate)
		if len(bc.SampleWindow) > bc.WindowSize {
			bc.SampleWindow = bc.SampleWindow[1:] // удаляем самый старый замер
		}

		// Вычисляем среднее по окну
		var sum int64
		for _, b := range bc.SampleWindow {
			sum += b
		}
		bc.CurrentBitrate = sum / int64(len(bc.SampleWindow))

		// Сбрасываем окно
		bc.WindowStartTime = time.Now()
		bc.WindowBytes = 0
	}
}

// GetBitrate возвращает текущий битрейт в бит/с
func (bc *BitrateCalculator) GetBitrate() int64 {
	if bc.CurrentBitrate == 0 {
		// Если еще не рассчитан, дать приблизительную оценку
		elapsed := time.Since(bc.StartTime).Seconds()
		if elapsed > 0 {
			return int64(float64(bc.BytesSent) * 8 / elapsed)
		}
	}
	return bc.CurrentBitrate
}

// GetTotalBytes возвращает общее количество отправленных байт
func (bc *BitrateCalculator) GetTotalBytes() int64 {
	return bc.BytesSent
}

func init() {
	// Registrar todos los formatos
	format.RegisterAll()
}

func main() {
	// Загрузить конфигурацию
	config, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("Ошибка загрузки конфигурации: %v", err)
	}

	// Устанавливаем минимальное время воспроизведения, если оно указано в конфигурации
	minFilePlayTime := minPlayTime
	if config.Settings.MinPlayTime > 0 {
		minFilePlayTime = time.Duration(config.Settings.MinPlayTime) * time.Second
		fmt.Printf("Установлено минимальное время воспроизведения: %v\n", minFilePlayTime)
	}

	videoDir := config.Video.Directory
	rtmpURL := config.RTMP.URL + config.RTMP.Key

	fmt.Println("=== MP4 RTMP Стример ===")
	fmt.Printf("RTMP URL: %s\n", rtmpURL)
	fmt.Printf("Директория видео: %s\n", videoDir)

	// Информация о настройках битрейта
	if config.Settings.ForceBitrate > 0 {
		fmt.Printf("Принудительный битрейт: %d kbps\n", config.Settings.ForceBitrate/1000)
	} else {
		fmt.Printf("Минимальный битрейт: %d kbps\n", minBitrate/1000)
	}
	if config.Settings.ForceKeyframe {
		fmt.Printf("Принудительная генерация ключевых кадров каждые %d сек\n", config.Settings.KeyframeSeconds)
	}
	if config.Settings.DisableEarlyEnd {
		fmt.Println("Раннее завершение файла отключено, каждый файл будет воспроизведен до конца")
	}
	if config.Settings.RestoreState {
		fmt.Println("Включено восстановление состояния из предыдущей сессии")
	}
	fmt.Println()

	// Первоначальное сканирование директории
	mp4Files := scanVideoDirectory(videoDir)
	if len(mp4Files) == 0 {
		log.Fatal("MP4 файлы не найдены в каталоге видео")
	}

	// Создаем общий калькулятор битрейта для всей сессии
	sessionBitrate := NewBitrateCalculator(10)

	// Проверяем существование и загружаем состояние, если необходимо
	var state *StreamState
	if config.Settings.RestoreState {
		var err error
		state, err = loadStreamState()
		if err != nil {
			log.Printf("Ошибка при загрузке состояния: %v. Начинаем с начала.", err)
		}
	}

	// Цикл непрерывного стриминга
	streamCount := 0
	fileIndex := 0
	consecutiveErrors := 0

	// Восстанавливаем fileIndex, если есть сохраненное состояние
	if state != nil && state.CurrentFile != "" {
		// Находим индекс файла в списке
		found := false
		for i, file := range mp4Files {
			if file.Name() == state.CurrentFile {
				fileIndex = i
				found = true
				fmt.Printf("🔄 Восстановление с файла #%d: %s, позиция: %v\n",
					fileIndex+1, state.CurrentFile, state.Position.Round(time.Second))
				break
			}
		}

		if !found {
			fmt.Printf("⚠️ Не найден файл из сохраненного состояния: %s. Начинаем с первого файла.\n", state.CurrentFile)
			state = nil // Сбрасываем состояние, если файл не найден
		}
	}

	// Текущее состояние для сохранения
	currentState := &StreamState{
		FileIndex: fileIndex,
	}

	// Создаем таймер для периодического сохранения состояния
	saveStateTicker := time.NewTicker(saveStateInterval)
	defer saveStateTicker.Stop()

	// Запускаем горутину для сохранения состояния
	go func() {
		for range saveStateTicker.C {
			// Проверяем, что есть какая-то информация для сохранения
			if currentState.CurrentFile != "" {
				err := saveStreamState(*currentState)
				if err != nil {
					log.Printf("Ошибка при сохранении состояния: %v", err)
				}
			}
		}
	}()

	for {
		streamCount++
		fmt.Printf("\n=== Цикл стриминга #%d ===\n", streamCount)

		// Повторное сканирование директории перед каждым циклом для обнаружения новых файлов
		mp4Files = scanVideoDirectory(videoDir)
		if len(mp4Files) == 0 {
			log.Println("⚠️ MP4 файлы не найдены, ожидание 5 секунд и повторная проверка...")
			time.Sleep(5 * time.Second)
			continue
		}

		for {
			// Проверяем, что индекс в допустимых пределах
			if fileIndex >= len(mp4Files) {
				fileIndex = 0
			}

			file := mp4Files[fileIndex]
			videoPath := filepath.Join(videoDir, file.Name())
			fmt.Printf("\n[%d/%d] Начало стриминга MP4: %s\n", fileIndex+1, len(mp4Files), file.Name())
			fmt.Printf("-> Отправка на RTMP: %s\n", rtmpURL)
			fmt.Printf("Текущий общий битрейт сессии: %d kbps\n", sessionBitrate.GetBitrate()/1000)

			// Обновляем информацию о текущем файле в состоянии
			currentState.CurrentFile = file.Name()
			currentState.LastSaveTime = time.Now()
			currentState.FileIndex = fileIndex

			// Определяем, нужно ли использовать начальную позицию для продолжения
			var startPosition time.Duration = 0
			if state != nil && state.CurrentFile == file.Name() {
				startPosition = state.Position
				fmt.Printf("▶️ Возобновление воспроизведения с позиции: %v\n", startPosition.Round(time.Second))
				// Сбрасываем состояние, чтобы больше не использовать его
				state = nil
			}

			// Попытки трансляции с повторами при ошибках
			var streamStatus StreamStatus
			var streamErr error

			startTime := time.Now()

			// Определяем, нужно ли использовать принудительный битрейт
			targetBitrate := minBitrate
			if config.Settings.ForceBitrate > 0 {
				targetBitrate = config.Settings.ForceBitrate
			}

			for attempt := 1; attempt <= maxRetries; attempt++ {
				if attempt > 1 {
					fmt.Printf("⚠️ Повторная попытка %d из %d...\n", attempt, maxRetries)
					time.Sleep(time.Duration(retryDelay) * time.Second)
				}

				// Передаем информацию о желаемом битрейте, калькулятор и начальную позицию
				streamStatus, streamErr = streamFileToRTMP(videoPath, rtmpURL, sessionBitrate,
					targetBitrate, config, minFilePlayTime, startPosition, currentState)
				duration := time.Since(startTime)

				if streamErr == nil {
					// Если streamStatus.PrepareNext = true, значит мы заранее вышли для подготовки следующего файла
					if streamStatus.PrepareNext {
						fmt.Printf("🔄 Файл %s почти закончен (длительность: %v), подготовка к следующему файлу\n",
							file.Name(), duration)
					} else {
						fmt.Printf("✅ Стриминг файла %s завершен (длительность: %v)\n", file.Name(), duration)
					}
					// Сбрасываем счетчик ошибок при успешной передаче
					consecutiveErrors = 0
					break
				} else {
					log.Printf("❌ Попытка %d: Ошибка при стриминге: %v", attempt, streamErr)
					consecutiveErrors++
				}
			}

			// Если слишком много ошибок подряд, делаем паузу и считаем, что нужно переподключиться
			if consecutiveErrors >= maxConsecutiveErrors {
				log.Printf("⛔ Слишком много ошибок подряд (%d). Пауза на %v и сброс соединения...",
					consecutiveErrors, reconnectTimeout)
				time.Sleep(reconnectTimeout)
				consecutiveErrors = 0
			}

			if streamErr != nil {
				log.Printf("⛔ Все попытки стриминга файла %s не удались. Переход к следующему файлу...", file.Name())
			} else {
				// Выводим информацию о битрейте после успешной передачи
				fmt.Printf("📊 Битрейт трансляции: %d kbps, отправлено: %.2f MB\n",
					streamStatus.Bitrate/1000, float64(sessionBitrate.GetTotalBytes())/(1024*1024))
			}

			// Сохраняем состояние после завершения файла
			err := saveStreamState(*currentState)
			if err != nil {
				log.Printf("Ошибка при сохранении состояния: %v", err)
			}

			// Если мы почти закончили файл и собираемся подготовить следующий,
			// пересканируем директорию, чтобы найти новые файлы
			if streamStatus.PrepareNext {
				fmt.Println("🔍 Сканирование директории на наличие новых файлов...")
				newMp4Files := scanVideoDirectory(videoDir)

				if len(newMp4Files) > len(mp4Files) {
					fmt.Printf("📁 Обнаружены новые файлы! Было: %d, стало: %d\n",
						len(mp4Files), len(newMp4Files))
					mp4Files = newMp4Files
				}
			}

			// Переходим к следующему файлу
			fileIndex++
			// Сбрасываем текущую позицию, так как будет новый файл
			currentState.Position = 0

			if fileIndex >= len(mp4Files) {
				fileIndex = 0
				fmt.Println("\n🔄 Все файлы проиграны, начинаем заново...")
				// Перед новым циклом делаем небольшую паузу для стабильности
				time.Sleep(1 * time.Second)
				break // Завершаем внутренний цикл, чтобы начать новый с обновленным списком файлов
			}
		}
	}
}

// Сканирование директории и получение списка MP4 файлов
func scanVideoDirectory(videoDir string) []os.DirEntry {
	// Поиск видеофайлов
	files, err := os.ReadDir(videoDir)
	if err != nil {
		log.Printf("Ошибка при чтении каталога видео: %v", err)
		return nil
	}

	// Фильтр только MP4 файлов
	var mp4Files []os.DirEntry
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(strings.ToLower(file.Name()), ".mp4") {
			mp4Files = append(mp4Files, file)
		}
	}

	if len(mp4Files) == 0 {
		return nil
	}

	// Сортировка файлов по имени для предсказуемого порядка
	sort.Slice(mp4Files, func(i, j int) bool {
		return mp4Files[i].Name() < mp4Files[j].Name()
	})

	fmt.Printf("Найдено %d MP4 файлов для стриминга\n", len(mp4Files))

	// Информация о файлах
	for _, file := range mp4Files {
		path := filepath.Join(videoDir, file.Name())
		info, err := os.Stat(path)
		if err == nil {
			fmt.Printf("Файл: %s, Размер: %.2f MB\n", file.Name(), float64(info.Size())/(1024*1024))
		}
	}

	return mp4Files
}

// Загрузка конфигурации из файла
func loadConfig(configPath string) (*Config, error) {
	// Значения по умолчанию
	config := &Config{}
	config.Settings.KeyframeSeconds = 2       // Ключевой кадр каждые 2 секунды по умолчанию
	config.Settings.ReconnectOnNewFile = true // По умолчанию переподключаемся при каждом новом файле
	config.Settings.DisableEarlyEnd = false   // По умолчанию раннее завершение файла включено
	config.Settings.MinPlayTime = 60          // Минимум 60 секунд воспроизведения по умолчанию
	config.Settings.RestoreState = true       // По умолчанию восстанавливаем состояние при запуске

	file, err := os.Open(configPath)
	if err != nil {
		// Если файл не найден, создаем его с дефолтными значениями
		if os.IsNotExist(err) {
			config.RTMP.URL = "rtmp://ovsu.okcdn.ru/input/"
			config.RTMP.Key = "-230262285_889404346_43_bseha6vkqe"
			config.Video.Directory = "video"
			config.Video.LoopMode = true

			// Сохраняем дефолтный файл
			jsonData, _ := json.MarshalIndent(config, "", "  ")
			os.WriteFile(configPath, jsonData, 0644)

			return config, nil
		}
		return nil, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// Пакет с информацией о времени и потоке
type TimedPacket struct {
	Packet    av.Packet
	SendAfter time.Time
	IsAudio   bool
}

func streamFileToRTMP(videoPath, rtmpURL string, bitrateCalc *BitrateCalculator, targetBitrate int, config *Config, minPlayTime time.Duration, startPosition time.Duration, state *StreamState) (StreamStatus, error) {
	// Инициализация статуса
	status := StreamStatus{
		EndOfFile:    false,
		PrepareNext:  false,
		TotalPackets: 0,
		Bitrate:      0,
	}

	// Счетчик попыток исправления файла
	var fixAttempts int = 0

tryAgain:
	// Открыть видеофайл
	fmt.Println("Открытие MP4 файла...")
	var file av.DemuxCloser
	var err error

	// Открываем MP4 файл
	file, err = avutil.Open(videoPath)
	if err != nil {
		// Проверяем, не связана ли ошибка с отсутствием атома moov
		if strings.Contains(err.Error(), "moov") {
			if fixAttempts < 2 {
				fmt.Printf("⚠️ Обнаружена ошибка структуры MP4 (отсутствует атом 'moov'), попытка исправления (%d/2)...\n", fixAttempts+1)

				fixAttempts++
				err = fixMP4Structure(videoPath)
				if err != nil {
					log.Printf("❌ Не удалось исправить структуру MP4 файла: %v\n", err)
				} else {
					fmt.Println("✅ Структура MP4 файла исправлена, повторная попытка открытия...")
					time.Sleep(1 * time.Second)
					goto tryAgain
				}
			}
		}
		return status, fmt.Errorf("ошибка при открытии MP4 файла: %v", err)
	}
	defer file.Close()

	// Подключение к RTMP серверу с увеличенным таймаутом
	fmt.Println("Подключение к RTMP серверу...")
	rtmpConn, err := rtmp.Dial(rtmpURL)
	if err != nil {
		return status, fmt.Errorf("ошибка при подключении к RTMP серверу: %v", err)
	}
	defer rtmpConn.Close()

	// Получение информации о потоках
	fmt.Println("Получение информации о потоках...")
	streams, err := file.Streams()
	if err != nil {
		// Проверяем, не связана ли ошибка с отсутствием атома moov
		if strings.Contains(err.Error(), "moov") && fixAttempts < 2 {
			fmt.Printf("⚠️ Ошибка структуры MP4 (отсутствует атом 'moov') при получении потоков, попытка исправления (%d/2)...\n", fixAttempts+1)
			file.Close()
			rtmpConn.Close()

			fixAttempts++
			err = fixMP4Structure(videoPath)
			if err != nil {
				log.Printf("❌ Не удалось исправить структуру MP4 файла: %v\n", err)
				return status, fmt.Errorf("ошибка при получении потоков: %v", err)
			}

			fmt.Println("✅ Структура MP4 файла исправлена, пробуем открыть снова...")
			goto tryAgain
		}
		return status, fmt.Errorf("ошибка при получении потоков: %v", err)
	}

	// Анализ потоков и идентификация аудио/видео индексов
	var audioStreamIdx, videoStreamIdx int = -1, -1
	fmt.Printf("Информация о потоках:\n")
	for i, stream := range streams {
		streamType := stream.Type().String()
		fmt.Printf("  Поток #%d: %s\n", i, streamType)

		if streamType == "H264" || streamType == "Video" {
			videoStreamIdx = i
			fmt.Printf("  Видео кодек: %s, Разрешение: %dx%d\n",
				streamType,
				stream.(av.VideoCodecData).Width(),
				stream.(av.VideoCodecData).Height())
		} else if streamType == "AAC" || streamType == "Audio" {
			audioStreamIdx = i
			if audioStream, ok := stream.(av.AudioCodecData); ok {
				fmt.Printf("  Аудио кодек: %s, Частота: %d Гц, Каналы: %d\n",
					streamType,
					audioStream.SampleRate(),
					audioStream.ChannelLayout().Count())
			}
		}
	}

	fmt.Printf("Обнаружены потоки: Видео=%d, Аудио=%d\n", videoStreamIdx, audioStreamIdx)

	// Проверяем, что нашли хотя бы один поток
	if videoStreamIdx == -1 && audioStreamIdx == -1 {
		return status, fmt.Errorf("не найдены аудио или видео потоки в файле")
	}

	// Установка заголовков потоков для RTMP
	fmt.Println("Запись заголовка потока...")
	err = rtmpConn.WriteHeader(streams)
	if err != nil {
		return status, fmt.Errorf("ошибка при записи заголовка: %v", err)
	}

	// Создаем калькулятор битрейта для этого файла
	fileBitrate := NewBitrateCalculator(5)

	// Если у нас есть начальная позиция, пытаемся перемотать к этой позиции
	if startPosition > 0 {
		fmt.Printf("📍 Перемотка к позиции %v...\n", startPosition.Round(time.Second))
	}

	// Запускаем потоковую передачу пакетов
	return streamPacketsSync(file, rtmpConn, audioStreamIdx, videoStreamIdx, fileBitrate, bitrateCalc, targetBitrate, config, minPlayTime, startPosition, state)
}

// fixMP4Structure пытается исправить структуру MP4 файла с отсутствующим атомом 'moov'
func fixMP4Structure(videoPath string) error {
	fmt.Println("🔧 Начало исправления структуры MP4 файла...")

	// Создаем временный файл для исправленного видео
	tmpPath := videoPath + ".fixed.mp4"

	// Для исправления MP4 файлов мы используем внешний инструмент ffmpeg
	// ffmpeg может автоматически переупаковать MP4 и исправить отсутствующий атом moov

	// Проверяем наличие ffmpeg в системе
	cmd := exec.Command("ffmpeg", "-version")
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("для исправления MP4 требуется ffmpeg, но он не найден в системе: %v", err)
	}

	fmt.Println("🔍 Анализ MP4 файла с помощью ffmpeg...")

	// Запускаем ffmpeg для анализа файла
	analyzeCmd := exec.Command("ffmpeg", "-v", "error", "-i", videoPath)
	output, _ := analyzeCmd.CombinedOutput()

	if len(output) > 0 {
		fmt.Printf("⚠️ Обнаружены проблемы в файле: %s\n", string(output))
	}

	// Запускаем ffmpeg для ремонта файла - переупаковываем без перекодирования
	fmt.Println("🛠️ Исправление структуры MP4 файла с помощью ffmpeg...")

	// Формируем команду для ремонта файла
	// -c copy = копирование потоков без перекодирования
	// -movflags faststart = перемещение атома moov в начало файла для быстрого старта
	remuxCmd := exec.Command("ffmpeg",
		"-v", "warning", // Уровень вывода (только предупреждения)
		"-i", videoPath, // Входной файл
		"-c", "copy", // Копировать все потоки без перекодирования
		"-map", "0", // Использовать все потоки из источника
		"-movflags", "faststart", // Оптимизация расположения атомов для быстрого старта
		"-f", "mp4", // Формат MP4
		tmpPath) // Выходной файл

	remuxOutput, err := remuxCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ошибка при ремонте MP4 файла: %v\n%s", err, string(remuxOutput))
	}

	// Создаем бэкап оригинального файла
	backupPath := videoPath + ".bak"
	err = os.Rename(videoPath, backupPath)
	if err != nil {
		return fmt.Errorf("ошибка при создании бэкапа оригинального файла: %v", err)
	}

	// Заменяем оригинальный файл исправленным
	err = os.Rename(tmpPath, videoPath)
	if err != nil {
		// Если не удалось, пытаемся восстановить оригинал
		os.Rename(backupPath, videoPath)
		return fmt.Errorf("ошибка при замене оригинального файла: %v", err)
	}

	fmt.Println("✅ MP4 файл успешно исправлен и сохранен")
	return nil
}

// Синхронизированная потоковая передача пакетов
func streamPacketsSync(file av.DemuxCloser, rtmpConn *rtmp.Conn, audioIdx, videoIdx int,
	fileBitrate, sessionBitrate *BitrateCalculator, targetBitrate int, config *Config, minPlayTime time.Duration,
	startPosition time.Duration, state *StreamState) (StreamStatus, error) {
	fmt.Println("Начало синхронизированной передачи пакетов...")

	// Инициализация статуса
	status := StreamStatus{
		EndOfFile:    false,
		PrepareNext:  false,
		TotalPackets: 0,
		Bitrate:      0,
	}

	// Инициализация переменных
	startTime := time.Now()
	totalPackets := 0
	totalBytes := int64(0)

	// Детекторы для первых таймстампов
	var firstVideoTS, firstAudioTS time.Duration = -1, -1
	var lastVideoTS, lastAudioTS time.Duration

	// Переменные для определения, когда пора подготовить следующий файл
	var videoDuration time.Duration
	var endDetected bool

	// Предотвращаем раннее завершение при коротких файлах
	minTimeReached := false

	// Флаг для отслеживания, нужно ли нам пропускать пакеты
	skipToPosition := startPosition > 0
	var skipUntilPos time.Duration
	var skipStarted bool

	// Таймстампы реального времени для синхронизации
	baseRealTime := time.Now()

	// Время последнего принудительного ключевого кадра
	lastKeyframeTime := time.Now()

	// Статистика для мониторинга производительности
	lastStatusTime := time.Now()
	lastStateSaveTime := time.Now()
	var statusInterval time.Duration = 5 * time.Second
	var stateSaveInterval time.Duration = 30 * time.Second

	// Создадим канал для таймера минимального времени воспроизведения
	minPlayTimeTimer := time.NewTimer(minPlayTime)
	go func() {
		<-minPlayTimeTimer.C
		minTimeReached = true
		fmt.Printf("⏱️ Достигнуто минимальное время воспроизведения: %v\n", minPlayTime)
	}()
	defer minPlayTimeTimer.Stop()

	for {
		pkt, err := file.ReadPacket()
		if err != nil {
			if err == io.EOF {
				fmt.Println("Конец файла, стрим завершен")
				status.EndOfFile = true
				break
			}
			return status, fmt.Errorf("ошибка чтения пакета: %v", err)
		}

		totalPackets++
		packetSize := int64(len(pkt.Data))
		totalBytes += packetSize

		// Обновляем калькуляторы битрейта
		fileBitrate.AddBytes(packetSize)
		sessionBitrate.AddBytes(packetSize)

		isAudio := int(pkt.Idx) == audioIdx
		isVideo := int(pkt.Idx) == videoIdx

		// Инициализируем первые таймстампы для аудио и видео отдельно
		if isVideo && firstVideoTS < 0 {
			firstVideoTS = pkt.Time
			lastVideoTS = pkt.Time
			fmt.Printf("Первый видео таймстамп: %v\n", firstVideoTS)

			// Устанавливаем позицию для пропуска пакетов
			if skipToPosition {
				skipUntilPos = firstVideoTS + startPosition
				fmt.Printf("Пропуск пакетов до позиции: %v\n", skipUntilPos)
				skipStarted = true
			}
		} else if isAudio && firstAudioTS < 0 {
			firstAudioTS = pkt.Time
			lastAudioTS = pkt.Time
			fmt.Printf("Первый аудио таймстамп: %v\n", firstAudioTS)
		}

		// Если оба первых таймстампа еще не обнаружены, просто отправляем пакеты без задержки
		if firstVideoTS < 0 || firstAudioTS < 0 {
			err = rtmpConn.WritePacket(pkt)
			if err != nil {
				return status, fmt.Errorf("ошибка отправки начального пакета: %v", err)
			}
			continue
		}

		// Вычисляем время воспроизведения относительно первого таймстампа соответствующего потока
		var streamPos time.Duration

		if isVideo {
			streamPos = pkt.Time - firstVideoTS
			lastVideoTS = pkt.Time
			videoDuration = streamPos

			// Принудительная установка флага ключевого кадра, если это настроено
			// и прошло достаточно времени с последнего ключевого кадра
			if config.Settings.ForceKeyframe &&
				time.Since(lastKeyframeTime) > time.Duration(config.Settings.KeyframeSeconds)*time.Second {
				pkt.IsKeyFrame = true
				lastKeyframeTime = time.Now()
			}

			// Обновляем текущую позицию в состоянии
			if state != nil {
				state.Position = streamPos
			}
		} else if isAudio {
			streamPos = pkt.Time - firstAudioTS
			lastAudioTS = pkt.Time
		} else {
			// Для других потоков используем видео таймстамп
			streamPos = pkt.Time - firstVideoTS
		}

		// Отслеживаем максимальный таймстамп как продолжительность
		if streamPos > videoDuration {
			videoDuration = streamPos
		}

		// Если нужно пропустить пакеты до определенной позиции (восстановление состояния)
		if skipStarted && (pkt.Time < skipUntilPos) {
			// Просто пропускаем эти пакеты
			continue
		} else if skipStarted {
			skipStarted = false
			fmt.Printf("📍 Достигнута начальная позиция %v, начинаем передачу\n", streamPos.Round(time.Second))
			// Переустанавливаем базовое время, чтобы синхронизация начиналась с текущего момента
			baseRealTime = time.Now().Add(-streamPos)
		}

		// Точное время, когда пакет должен быть отправлен
		targetSendTime := baseRealTime.Add(streamPos)

		// Вычисляем, сколько нужно подождать
		waitTime := targetSendTime.Sub(time.Now())

		// Добавляем проверку на отрицательное время (если отстаем) и слишком большое время (если что-то пошло не так)
		if waitTime > 0 && waitTime < 500*time.Millisecond {
			time.Sleep(waitTime)
		} else if waitTime > 500*time.Millisecond {
			// Если задержка слишком большая, корректируем базовое время
			fmt.Printf("⚠️ Большая задержка обнаружена (%v), перекалибровка\n", waitTime)
			baseRealTime = time.Now().Add(-streamPos)
		}

		// Отправляем пакет
		err = rtmpConn.WritePacket(pkt)
		if err != nil {
			return status, fmt.Errorf("ошибка отправки пакета: %v", err)
		}

		// Периодическое сохранение состояния
		if state != nil && time.Since(lastStateSaveTime) > stateSaveInterval {
			lastStateSaveTime = time.Now()
			err := saveStreamState(*state)
			if err != nil {
				log.Printf("Ошибка при сохранении состояния: %v", err)
			}
		}

		// Определяем, должны ли мы начать подготовку следующего файла
		// Проверяем, что прошло минимальное время воспроизведения и пользователь не отключил раннее завершение
		if isVideo && !endDetected && minTimeReached && !config.Settings.DisableEarlyEnd {
			// Проверяем, можем ли мы определить приближение конца файла
			if pkt.IsKeyFrame && videoDuration > preloadNextFileTime {
				elapsedTime := time.Since(startTime)

				// Определяем оставшееся время более точно
				// Используем метаданные файла, если они доступны, иначе приближенные вычисления
				estimatedRemaining := time.Duration(0)

				// Если файл воспроизводится достаточно долго, можно использовать отношение времени
				if elapsedTime > 30*time.Second && streamPos > 0 {
					elapsedRatio := float64(elapsedTime) / float64(streamPos)
					estimatedRemaining = time.Duration(float64(videoDuration-streamPos) * elapsedRatio)

					// Устанавливаем флаг подготовки следующего файла, если осталось мало времени
					if estimatedRemaining < preloadNextFileTime {
						fmt.Printf("🔍 Приближается конец файла! Прошло: %v, Текущая позиция: %v, Осталось ~%v\n",
							elapsedTime.Round(time.Second), streamPos.Round(time.Second), estimatedRemaining.Round(time.Second))
						status.PrepareNext = true
						endDetected = true
					}
				}
			}
		}

		// Проверка на принудительное завершение, только если не отключено раннее завершение
		// и прошло минимальное время воспроизведения
		if status.PrepareNext && minTimeReached && !config.Settings.DisableEarlyEnd {
			// Задержка для стабильности
			if elapsedReal := time.Since(startTime); elapsedReal > minPlayTime {
				fmt.Printf("🔄 Заблаговременное завершение трансляции после %v для подготовки следующего файла\n",
					elapsedReal.Round(time.Second))
				break
			}
		}

		// Периодический вывод статистики битрейта
		if time.Since(lastStatusTime) > statusInterval {
			currentBitrate := fileBitrate.GetBitrate()
			elapsed := time.Since(startTime)

			videoProgress := ""
			audioProgress := ""

			if firstVideoTS >= 0 && lastVideoTS > firstVideoTS {
				videoProgress = fmt.Sprintf("Видео: %v", lastVideoTS-firstVideoTS)
			}

			if firstAudioTS >= 0 && lastAudioTS > firstAudioTS {
				audioProgress = fmt.Sprintf("Аудио: %v", lastAudioTS-firstAudioTS)
			}

			fmt.Printf("  ▶️ Отправлено пакетов: %d | Битрейт: %d kbps | Время: %v | %s | %s\n",
				totalPackets, currentBitrate/1000, elapsed.Round(time.Second), videoProgress, audioProgress)

			lastStatusTime = time.Now()

			// Проверка на достаточность битрейта
			if currentBitrate < int64(minBitrate) {
				fmt.Printf("⚠️ Внимание! Текущий битрейт (%d kbps) ниже рекомендуемого (%d kbps)\n",
					currentBitrate/1000, minBitrate/1000)
			}
		}
	}

	// Заполняем итоговую статистику
	status.TotalPackets = totalPackets
	status.ElapsedTime = time.Since(startTime)
	status.VideoDuration = videoDuration
	status.Bitrate = fileBitrate.GetBitrate()

	// Вычисляем средний битрейт за всю передачу
	avgBitrate := int64(float64(totalBytes*8) / status.ElapsedTime.Seconds())

	fmt.Printf("Стриминг завершен. Пакетов: %d | Длительность: %v | Битрейт: %d kbps\n",
		totalPackets, status.ElapsedTime.Round(time.Second), avgBitrate/1000)
	return status, nil
}

// Сохранение состояния стрима в файл
func saveStreamState(state StreamState) error {
	state.LastSaveTime = time.Now()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("ошибка при преобразовании состояния в JSON: %v", err)
	}

	err = os.WriteFile(stateFilePath, data, 0644)
	if err != nil {
		return fmt.Errorf("ошибка при сохранении состояния в файл: %v", err)
	}

	fmt.Printf("💾 Состояние стрима сохранено: Файл %s, Позиция %v\n",
		state.CurrentFile, state.Position.Round(time.Second))
	return nil
}

// Загрузка состояния стрима из файла
func loadStreamState() (*StreamState, error) {
	data, err := os.ReadFile(stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Файл не существует, это нормально
		}
		return nil, fmt.Errorf("ошибка при чтении файла состояния: %v", err)
	}

	var state StreamState
	err = json.Unmarshal(data, &state)
	if err != nil {
		return nil, fmt.Errorf("ошибка при разборе JSON состояния: %v", err)
	}

	// Проверяем, не устарело ли состояние (например, больше недели)
	if time.Since(state.LastSaveTime) > 7*24*time.Hour {
		fmt.Println("⚠️ Сохраненное состояние устарело (больше недели), начинаем с начала")
		return nil, nil
	}

	fmt.Printf("📂 Загружено состояние стрима: Файл %s, Позиция %v\n",
		state.CurrentFile, state.Position.Round(time.Second))
	return &state, nil
}
