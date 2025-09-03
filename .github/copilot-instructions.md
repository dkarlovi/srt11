# srt11 - Subtitle to Audio Converter

Always reference these instructions first and fallback to search or bash commands only when you encounter unexpected information that does not match the info here.

## Working Effectively

### Prerequisites and Setup
- **Go version**: Requires Go 1.23.0 or later (check with `go version`)
- **Operating System**: Linux, macOS, or Windows (cross-platform)
- **API Requirements**: ElevenLabs API key required for actual TTS generation

### Build and Development Workflow
1. **Initial setup from fresh clone**:
   ```bash
   cd /path/to/srt11
   go mod tidy
   ```
   
2. **Build the application**:
   ```bash
   go build -o srt11 .
   ```
   - **First build time**: ~16 seconds (downloads dependencies)
   - **Subsequent builds**: ~0.5 seconds (cached dependencies)
   - **NEVER CANCEL** - Set timeout to 60+ seconds minimum for safety
   - **Output**: Creates `srt11` binary (Linux/macOS) or `srt11.exe` (Windows)
   - **Note**: Binary is automatically excluded by `.gitignore`

3. **Run tests** (NOTE: Currently no tests exist):
   ```bash
   go test -v ./...
   ```
   - Returns: "no test files"

### Code Quality and Linting
Always run these commands before committing changes:

1. **Go formatting** (auto-fixes formatting):
   ```bash
   go fmt ./...
   ```

2. **Go vet** (built-in static analysis):
   ```bash
   go vet ./...
   ```

3. **Optional advanced linting** (install if needed):
   ```bash
   # Install tools (one-time setup)
   go install golang.org/x/lint/golint@latest
   go install honnef.co/go/tools/cmd/staticcheck@latest
   
   # Run linting
   ~/go/bin/golint ./...
   ~/go/bin/staticcheck ./...
   ```

## Application Usage and Testing

### Configuration Setup
1. **Create config file** (required for operation):
   ```yaml
   # config.yaml
   auth_key: "sk_your_elevenlabs_api_key"
   default:
       model: "model_id_from_elevenlabs"
       name: "Default Speaker"
       speed: 1.0
   # Optional: merge lines if same speaker and gap below threshold (ms)
   merge_lines_threshold_ms: 50
   models:
       # Optional: add custom speakers
       Speaker2:
           model: "different_model_id"
           name: "Speaker2"
           speed: 1.1
   ```

2. **Test configuration** (without real API key):
   ```bash
   ./srt11 --help
   ```

### Sample Files for Testing
Create test SRT file for development:
```srt
1
00:00:00,000 --> 00:00:02,000
Hello, this is a test.

2
00:00:02,500 --> 00:00:04,500
This is the second line.

3
00:00:05,000 --> 00:00:07,000
[Speaker2]This is a different speaker.
```

### Running the Application
```bash
# Basic usage
./srt11 path/to/subtitle.srt

# With custom config
./srt11 -c custom_config.yaml path/to/subtitle.srt

# With line merging
./srt11 -m 100 path/to/subtitle.srt
```

**Expected behavior**: 
- Parses subtitle file
- Generates individual MP3 files for each line
- Creates final WAV file with merged audio
- **NOTE**: Will fail in CI/testing environments without valid ElevenLabs API key

## Validation Scenarios

### Manual Testing After Changes
1. **Build verification**:
   ```bash
   go build -o srt11 .
   ./srt11 --help
   ```

2. **Code formatting check**:
   ```bash
   go fmt ./...
   git diff --exit-code  # Should show no changes
   ```

3. **Static analysis**:
   ```bash
   go vet ./...
   ```

4. **Functionality test** (with dummy config):
   ```bash
   # Create test files
   cat > test.srt << 'EOF'
   1
   00:00:00,000 --> 00:00:02,000
   Hello, this is a test.
   
   2
   00:00:02,500 --> 00:00:04,500
   This is the second line.
   EOF
   
   cat > dummy_config.yaml << 'EOF'
   auth_key: "dummy_key_for_testing"
   default:
       model: "default_model_id" 
       name: "Default Speaker"
       speed: 1.0
   EOF
   
   # Test application
   ./srt11 -c dummy_config.yaml test.srt
   ```
   - **Expected result**: Shows "Speaking (as Default Speaker)" then fails at API call
   - **Should NOT fail at**: Config parsing or subtitle parsing stages
   - **Test merge functionality**: `./srt11 -c dummy_config.yaml -m 100 test.srt`

### CI/CD Pipeline
- **GitHub Actions**: `.github/workflows/build.yaml`
- **Platforms**: Builds for Linux, macOS, Windows
- **Triggers**: Push to main, pull requests, releases
- **Build time**: ~2-5 minutes per platform. NEVER CANCEL.
- **Artifacts**: Creates platform-specific ZIP files

## Repository Structure

### Key Files
```
.
├── README.md              # User documentation
├── go.mod                 # Go module definition
├── go.sum                 # Go dependency checksums
├── srt11.go              # Main application (single file)
├── config.yaml.dist      # Sample configuration file
├── .github/
│   └── workflows/
│       └── build.yaml    # CI/CD pipeline
└── .gitignore           # Excludes build artifacts
```

### Important Code Sections
- **Lines 66-78**: `Config` struct - Configuration file format
- **Lines 109-116**: `Options` struct - CLI argument parsing
- **Lines 188-321**: `parseSubtitleFile()` - Core subtitle parsing logic
- **Lines 323-401**: `generateMissingVoiceLines()` - ElevenLabs TTS integration
- **Lines 419-495**: `generateFinalAudioFile()` - Audio mixing logic
- **Lines 497-600**: `main()` - Application entry point

## Common Development Tasks

### Adding New Features
1. **Always format and vet first**:
   ```bash
   go fmt ./... && go vet ./...
   ```

2. **Build and test**:
   ```bash
   go build -o srt11 .
   ./srt11 --help
   ```

3. **Test with sample data** (functionality validation)

### Debugging Issues
1. **Check build errors**:
   ```bash
   go build -v .
   ```

2. **Run with verbose output**:
   ```bash
   ./srt11 -c config.yaml -m 0 test.srt
   ```

3. **Common issues**:
   - Missing config file: Create `config.yaml` from `config.yaml.dist`
   - API errors: Expected in test environments without valid keys
   - File format errors: Check SRT/VTT file formatting

### Performance Considerations
- **Single-file application**: All code in `srt11.go`
- **Fast builds**: ~16 seconds typical build time
- **Memory usage**: Depends on subtitle file size and audio generation
- **Network dependency**: Requires internet access for ElevenLabs API

## Dependencies and External Services

### Go Dependencies (managed by go.mod)
- `github.com/asticode/go-astisub` - Subtitle file parsing
- `github.com/go-audio/audio` - Audio processing
- `github.com/go-audio/wav` - WAV file generation
- `github.com/haguro/elevenlabs-go` - ElevenLabs API client (custom fork)
- `github.com/hajimehoshi/go-mp3` - MP3 audio decoding
- `github.com/jessevdk/go-flags` - CLI argument parsing
- `gopkg.in/yaml.v3` - YAML configuration parsing

### External Services
- **ElevenLabs API**: Text-to-speech generation (requires API key)
- **Internet access**: Required for TTS API calls

## Troubleshooting

### Build Issues
- **"go: module not found"**: Run `go mod tidy`
- **"permission denied"**: Check file permissions on binary
- **Version conflicts**: Ensure Go 1.23+ is installed

### Runtime Issues
- **"config file not found"**: Create `config.yaml` from template
- **"API key invalid"**: Check ElevenLabs API key in config
- **"file format error"**: Verify SRT/VTT file formatting
- **Network errors**: Expected in restricted environments

### Development Environment
- **No test files**: This is expected - application has no unit tests
- **Linting warnings**: Use golint/staticcheck for code quality checks
- **Build artifacts**: Binary files are gitignored automatically