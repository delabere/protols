package lsp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kralicky/protocompile"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protols/pkg/format"
	"github.com/kralicky/tools-lite/gopls/pkg/cache"
	"github.com/kralicky/tools-lite/gopls/pkg/file"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

type ImportSource int

const (
	SourceWellKnown ImportSource = iota + 1
	SourceRelativePath
	SourceLocalGoModule
	SourceGoModuleCache
	SourceSynthetic
)

type Resolver struct {
	*cache.OverlayFS
	fsDelegate                 *cache.MemoizedFS
	folder                     protocol.WorkspaceFolder
	goLanguageDriver           *GoLanguageDriver
	pathsMu                    sync.RWMutex
	filePathsByURI             map[protocol.DocumentURI]string // URI -> canonical file path (go package + file name)
	fileURIsByPath             map[string]protocol.DocumentURI // canonical file path (go package + file name) -> URI
	importSourcesByURI         map[protocol.DocumentURI]ImportSource
	syntheticFileOriginalNames map[protocol.DocumentURI]string
	syntheticFiles             map[protocol.DocumentURI]string
}

func NewResolver(folder protocol.WorkspaceFolder) *Resolver {
	fsDelegate := cache.NewMemoizedFS()
	return &Resolver{
		folder:                     folder,
		OverlayFS:                  cache.NewOverlayFS(fsDelegate),
		fsDelegate:                 fsDelegate,
		goLanguageDriver:           NewGoLanguageDriver(protocol.DocumentURI(folder.URI).Path()),
		filePathsByURI:             make(map[protocol.DocumentURI]string),
		fileURIsByPath:             make(map[string]protocol.DocumentURI),
		syntheticFileOriginalNames: make(map[protocol.DocumentURI]string),
		syntheticFiles:             make(map[protocol.DocumentURI]string),
		importSourcesByURI:         map[protocol.DocumentURI]ImportSource{},
	}
}

func (r *Resolver) OpenFileFromDisk(ctx context.Context, uri protocol.DocumentURI) (file.Handle, error) {
	return r.fsDelegate.ReadFile(ctx, uri)
}

func (r *Resolver) PathToURI(path string) (protocol.DocumentURI, error) {
	r.pathsMu.RLock()
	defer r.pathsMu.RUnlock()

	if i := strings.IndexRune(path, ';'); i != -1 {
		path = path[:i] // strip trailing ;packagename directive
	}

	uri, ok := r.fileURIsByPath[path]
	if !ok {
		return "", fmt.Errorf("%w: path %q", os.ErrNotExist, path)
	}
	return uri, nil
}

func (r *Resolver) URIToPath(uri protocol.DocumentURI) (string, error) {
	r.pathsMu.RLock()
	defer r.pathsMu.RUnlock()

	path, ok := r.filePathsByURI[uri]
	if !ok {
		return "", fmt.Errorf("%w: URI %q", os.ErrNotExist, uri)
	}
	return path, nil
}

func (r *Resolver) SyntheticFileContents(uri protocol.DocumentURI) (string, error) {
	r.pathsMu.RLock()
	defer r.pathsMu.RUnlock()
	contents, ok := r.syntheticFiles[uri]
	if !ok {
		return "", fmt.Errorf("%w: URI %q", os.ErrNotExist, uri)
	}
	return contents, nil
}

func (r *Resolver) UpdateURIPathMappings(modifications []file.Modification) {
	r.pathsMu.Lock()
	defer r.pathsMu.Unlock()
	for _, m := range modifications {
		switch m.Action {
		case file.Close:
		case file.Change, file.Save:
			// check for go_package modification
			if r.importSourcesByURI[m.URI] == SourceLocalGoModule {
				existingPath := r.filePathsByURI[m.URI]
				filename := m.URI.Path()
				var f io.ReadCloser
				if m.Text != nil {
					f = io.NopCloser(bytes.NewReader(m.Text))
				} else {
					var err error
					f, err = os.Open(filename)
					if err != nil {
						slog.With(
							"filename", filename,
							"error", err,
						).Error("failed to open file")
						continue
					}
				}
				mod, err := r.LookupGoModule(filename, f)
				f.Close()
				if err != nil {
					if err == ErrNoModule {
						slog.With("filename", filename).Info("go module is no longer present for file, removing from cache")
						delete(r.filePathsByURI, m.URI)
						delete(r.fileURIsByPath, existingPath)
						delete(r.importSourcesByURI, m.URI)
						continue
					}
					slog.With(
						"filename", filename,
						"error", err,
					).Error("failed to lookup go module")
					continue
				}
				updatedPath := filepath.Join(mod, filepath.Base(filename))
				if updatedPath != existingPath {
					slog.With(
						"existingPath", existingPath,
						"updatedPath", updatedPath,
					).Debug("updating path mapping")
					r.filePathsByURI[m.URI] = updatedPath
					r.fileURIsByPath[updatedPath] = m.URI
					if existingPath != "" {
						delete(r.fileURIsByPath, existingPath)
					}
				}
			}
		case file.Create:
			filename := m.URI.Path()
			f, err := os.Open(filename)
			if err != nil {
				slog.With(
					"filename", filename,
					"error", err,
				).Error("failed to open file")
				continue
			}
			goPkg, err := r.LookupGoModule(filename, f)
			f.Close()
			if err != nil {
				if err == ErrNoModule {
					relativePath := strings.TrimPrefix(m.URI.Path(), protocol.DocumentURI(r.folder.URI).Path()+"/")

					r.filePathsByURI[m.URI] = relativePath
					r.fileURIsByPath[relativePath] = m.URI
					r.importSourcesByURI[m.URI] = SourceRelativePath
					continue
				}
				slog.With(
					"filename", filename,
					"error", err,
				).Error("failed to lookup go module")
				continue
			}
			canonicalName := filepath.Join(goPkg, filepath.Base(filename))
			r.filePathsByURI[m.URI] = canonicalName
			r.fileURIsByPath[canonicalName] = m.URI
			r.importSourcesByURI[m.URI] = SourceLocalGoModule
		case file.Delete:
			path := r.filePathsByURI[m.URI]
			delete(r.filePathsByURI, m.URI)
			delete(r.importSourcesByURI, m.URI)
			delete(r.fileURIsByPath, path)
		case file.Open:
			// not necessarily a local go module

		}
	}
}

// CheckIncompleteDescriptors fills in placeholder sources for synthetic files
// that did not have fully linked descriptors at the time of creation, and
// returns a list of paths that need to be compiled again.
func (r *Resolver) CheckIncompleteDescriptors(results linker.Files) []string {
	r.pathsMu.Lock()
	defer r.pathsMu.Unlock()

	compileAgain := []string{}
	for uri, path := range r.filePathsByURI {
		if strings.HasPrefix(string(uri), "proto://") {
			if _, ok := r.syntheticFiles[uri]; !ok {
				res := results.FindFileByPath(path)
				if res == nil {
					continue
				}
				resolver := linker.ResolverFromFile(res)
				fdp := res.(linker.Result).FileDescriptorProto()
				data, err := proto.Marshal(fdp)
				if err != nil {
					slog.With(
						"uri", string(uri),
						"error", err,
					).Error("failed to generate synthetic file descriptor")
					continue
				}
				if err := (proto.UnmarshalOptions{Resolver: resolver}).Unmarshal(data, fdp); err != nil {
					slog.With(
						"uri", string(uri),
						"error", err,
					).Error("failed to generate synthetic file descriptor")
					continue
				}

				newFile, err := protodesc.FileOptions{
					AllowUnresolvable: true,
				}.New(fdp, resolver)
				if err != nil {
					slog.With(
						"uri", string(uri),
						"error", err,
					).Error("failed to generate synthetic file descriptor")
					// r.syntheticFiles[uri] = fmt.Sprintf("// failed to generate synthetic file descriptor: %s", err.Error())
					continue
				}
				var src bytes.Buffer
				err = format.PrintAndFormatFileDescriptor(newFile, &src)
				if err != nil {
					slog.With(
						"uri", string(uri),
						"error", err,
					).Error("failed to generate synthetic file source")
					continue
				}
				r.syntheticFiles[uri] = src.String()
				// these files aren't going to have ASTs yet and will need to be recompiled
				compileAgain = append(compileAgain, path)
			}
		}
	}
	return compileAgain
}

// Path resolution order:
// 1. Check for well-known import paths like google/*
// 2. Check if the path is a file on disk
// 3. Check if the path is a go module containing proto sources
// 3.5. Check if the path is a go module path containing generated code, but no proto sources
// 4. Check if the path is found in the global message cache
// 5. Try more complex path resolution strategies
// 6. Try to analyze existing generated code to find the path used to generate it
func (r *Resolver) FindFileByPath(path protocompile.UnresolvedPath, whence protocompile.ImportContext) (protocompile.SearchResult, error) {
	start := time.Now()
	r.pathsMu.Lock()
	defer r.pathsMu.Unlock()
	lockedTime := time.Since(start)
	if lockedTime >= 10*time.Millisecond {
		slog.Debug(fmt.Sprintf("warn: FindFileByPath blocked for %s", lockedTime))
	}
	res, err := r.findFileByPathLocked(string(path), whence)
	if err != nil {
		if whence != nil {
			translated, err2 := r.translatePathLocked(string(path), whence)
			if err2 == nil {
				slog.With("time", time.Since(start)).With("path", path, "translated", translated).Debug("resolved path by translation from import context")
				res, err2 = r.findFileByPathLocked(translated, whence)
				if err2 == nil {
					res.ResolvedPath = protocompile.ResolvedPath(translated)
					return res, nil
				}
			} else {
				rev, err3 := r.tryReverseLookupLocked(string(path), whence)
				if err3 == nil {
					slog.With("time", time.Since(start)).With("path", path, "resolved", rev).Debug("resolved path by reverse lookup")
					res, err3 = r.findFileByPathLocked(rev, whence)
					if err3 == nil {
						res.ResolvedPath = protocompile.ResolvedPath(rev)
						return res, nil
					}
				}
			}

			err := errors.Join(err, err2)
			slog.With("time", time.Since(start)).With("path", path, "errs", err).Debug("could not resolve path")
			return protocompile.SearchResult{}, err
		}

		slog.With("time", time.Since(start)).With("path", path, "errs", err).Debug("could not resolve path")
		return protocompile.SearchResult{}, err
	}

	return res, nil
}

func (r *Resolver) findFileByPathLocked(path string, whence protocompile.ImportContext) (protocompile.SearchResult, error) {
	start := time.Now()
	var isSynthetic bool
	if uri, ok := r.fileURIsByPath[path]; ok {
		if strings.HasPrefix(string(uri), "proto://") {
			isSynthetic = true
		}
	}
	lg := slog.With("path", path)
	if result, err := r.checkWellKnownImportPath(path); err == nil {
		lg.With("time", time.Since(start)).Debug("resolved to well-known import path")
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		lg.Error("failed to check well-known import path")
		return protocompile.SearchResult{}, err
	}
	if !isSynthetic {
		if result, err := r.checkFS(path, whence); err == nil {
			lg.With("time", time.Since(start)).Debug("resolved to cached file")
			return result, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			slog.With("path", path, "error", err).Debug("failed to check cached file")
			return protocompile.SearchResult{}, err
		}
	}

	if result, err := r.checkGoModule(path, whence); err == nil {
		lg.With("time", time.Since(start)).Debug("resolved to go module")
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrNoModule) {
		lg.Debug("failed to check go module")
		return protocompile.SearchResult{}, err
	}
	if IsWellKnownPath(path) {
		if result, err := r.checkGlobalCache(path); err == nil {
			lg.With("time", time.Since(start)).Debug("resolved to type in global descriptor cache")
			return result, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			lg.Debug("failed to check global descriptor cache")
			return protocompile.SearchResult{}, err
		}
	}

	if filepath.Base(path) == "gogo.proto" {
		if result, err := r.checkGoModule("github.com/gogo/protobuf/gogoproto/gogo.proto", whence); err == nil {
			lg.With("time", time.Since(start)).Debug("resolved to special case (go module: gogo.proto)")
			return result, nil
		}
	}

	return protocompile.SearchResult{}, os.ErrNotExist
}

func (r *Resolver) checkWellKnownImportPath(path string) (protocompile.SearchResult, error) {
	if IsWellKnownPath(path) {
		return r.checkGlobalCache(path)
	}
	return protocompile.SearchResult{}, os.ErrNotExist
}

const largeFileThreshold = 1024 * 1024 // 1MB

func (r *Resolver) checkFS(path string, whence protocompile.ImportContext) (protocompile.SearchResult, error) {
	uri, ok := r.fileURIsByPath[path]
	if ok {
		if fh, err := r.ReadFile(context.TODO(), uri); err == nil {
			content, err := fh.Content()
			if len(content) > largeFileThreshold {
				return protocompile.SearchResult{}, fmt.Errorf("refusing to load file %q larger than 1MB", path)
			}
			if err == nil && content != nil {
				return protocompile.SearchResult{
					ResolvedPath: protocompile.ResolvedPath(path),
					Version:      fh.Version(),
					Source:       bytes.NewReader(content),
				}, nil
			}
		}
	}

	return protocompile.SearchResult{}, os.ErrNotExist
}

func (r *Resolver) checkGoModule(path string, whence protocompile.ImportContext) (protocompile.SearchResult, error) {
	if !r.goLanguageDriver.HasGoModule() {
		return protocompile.SearchResult{}, ErrNoModule
	}
	if strings.HasPrefix(path, "github.com/gogo/googleapis/") {
		// these are vendored in the gogo/protobuf repo, so we need to special case them
		// to avoid conflicting symbols
		return r.checkWellKnownImportPath(strings.TrimPrefix(path, "github.com/gogo/googleapis/"))
	}
	res, err := r.goLanguageDriver.ImportFromGoModule(path)
	if err != nil {
		// TODO: if the path is not directly resolvable, refer to the import context?
		return protocompile.SearchResult{}, err
	}

	if res.SourceExists {
		src, err := os.Open(res.SourcePath)
		if err != nil {
			return protocompile.SearchResult{}, err
		}
		uri := protocol.URIFromPath(res.SourcePath)
		r.filePathsByURI[uri] = path
		r.fileURIsByPath[path] = uri
		if res.Module.Path == r.goLanguageDriver.localModName {
			r.importSourcesByURI[uri] = SourceLocalGoModule
		} else {
			r.importSourcesByURI[uri] = SourceGoModuleCache
		}
		return protocompile.SearchResult{
			Version:      1,
			ResolvedPath: protocompile.ResolvedPath(path),
			Source:       src, // this is closed by the compiler
		}, nil
	}

	var fileURI protocol.DocumentURI
	var usingAltPath bool
	if res.KnownAltPath != "" {
		fileURI = r.fileURIsByPath[res.KnownAltPath]
		usingAltPath = true
	} else {
		fileURI = r.fileURIsByPath[path]
	}
	if src, ok := r.syntheticFiles[fileURI]; ok {
		slog.With(
			"alt_path", usingAltPath,
			"uri", fileURI,
		).Debug("using cached synthetic file")
		var resolved string
		if res.KnownAltPath == "" {
			resolved = path
		} else {
			resolved = res.KnownAltPath
		}
		return protocompile.SearchResult{
			Version:      1,
			ResolvedPath: protocompile.ResolvedPath(resolved),
			Source:       strings.NewReader(src),
		}, nil
	} else {
		slog.With(
			"alt_path", usingAltPath,
			"uri", fileURI,
		).Debug("building new synthetic file")
	}

	start := time.Now()
	if synthesized, err := r.goLanguageDriver.SynthesizeFromGoSource(path, res); err == nil {
		var original, resolved string
		if res.KnownAltPath == "" {
			original = *synthesized.Name
			resolved = path
		} else {
			original = path
			resolved = res.KnownAltPath
		}
		syntheticURI := url.URL{
			Scheme:   "proto",
			Path:     resolved,
			Fragment: r.folder.Name,
		}
		uri := protocol.DocumentURI(syntheticURI.String())
		slog.With("time", time.Since(start)).
			With(
				"path", path,
				"synthetic_uri", uri,
				"resolved_path", resolved,
				"original_name", original,
			).
			Debug("synthesized proto from go source")
		r.filePathsByURI[uri] = resolved
		r.fileURIsByPath[resolved] = uri
		r.importSourcesByURI[uri] = SourceSynthetic
		r.syntheticFileOriginalNames[uri] = original
		return protocompile.SearchResult{
			Version:      1,
			ResolvedPath: protocompile.ResolvedPath(resolved),
			Proto:        synthesized,
		}, nil
	}
	return protocompile.SearchResult{}, fmt.Errorf("failed to synthesize %s: %w", path, err)
}

func (r *Resolver) checkGlobalCache(path string) (protocompile.SearchResult, error) {
	fd, err := protoregistry.GlobalFiles.FindFileByPath(path)
	if err != nil {
		return protocompile.SearchResult{}, err
	}
	syntheticURI := url.URL{
		Scheme:   "proto",
		Path:     path,
		Fragment: r.folder.Name,
	}
	if src, ok := r.syntheticFiles[protocol.DocumentURI(syntheticURI.String())]; ok {
		return protocompile.SearchResult{
			Version:      1,
			ResolvedPath: protocompile.ResolvedPath(path),
			Source:       strings.NewReader(src),
		}, nil
	}
	uri := protocol.DocumentURI(syntheticURI.String())
	r.filePathsByURI[uri] = path
	r.fileURIsByPath[path] = uri
	var src bytes.Buffer
	err = format.PrintAndFormatFileDescriptor(fd, &src)
	if err != nil {
		return protocompile.SearchResult{
			ResolvedPath: protocompile.ResolvedPath(path),
			Proto:        protodesc.ToFileDescriptorProto(fd),
		}, nil
	}
	r.syntheticFiles[uri] = src.String()
	return protocompile.SearchResult{
		ResolvedPath: protocompile.ResolvedPath(path),
		Source:       strings.NewReader(r.syntheticFiles[uri]),
	}, nil
}

func (r *Resolver) SyntheticFiles() []protocol.DocumentURI {
	var uris []protocol.DocumentURI
	for uri := range r.syntheticFiles {
		uris = append(uris, uri)
	}
	sort.Slice(uris, func(i, j int) bool {
		return string(uris[i]) < string(uris[j])
	})
	return uris
}

var ErrNoModule = errors.New("no go module found")

func (r *Resolver) LookupGoModule(filename string, f io.Reader) (string, error) {
	if !r.goLanguageDriver.HasGoModule() {
		return "", ErrNoModule
	}

	// Check if the filename is relative to a local go module
	if pkgName, err := r.goLanguageDriver.ImplicitGoPackagePath(filename); err == nil {
		return pkgName, nil
	}

	// If the file contains a go_module option, use that
	if mod, err := FastLookupGoModule(f); err == nil {
		return mod, nil
	}

	return "", fmt.Errorf("could not determine go module for %s", filename)
}

func (r *Resolver) IsRealWorkspaceLocalFile(uri protocol.DocumentURI) bool {
	if !uri.IsFile() {
		return false
	}

	r.pathsMu.RLock()
	defer r.pathsMu.RUnlock()

	// check if the file is synthetic
	if _, ok := r.syntheticFiles[uri]; ok {
		return false
	}

	// check if the file has a known import source indicating it is not a real file
	// or is outside the workspace root
	if src, ok := r.importSourcesByURI[uri]; ok {
		if src == SourceSynthetic || src == SourceGoModuleCache || src == SourceWellKnown {
			return false
		}
	}

	// check if the file is known to the fs
	_, err := r.ReadFile(context.Background(), uri)
	if err != nil {
		return false
	}
	filename := uri.Path()

	// check if the file is a symlink or otherwise not a regular file
	info, err := os.Lstat(filename)
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return false
	}

	// lastly, check if the file is within the workspace root
	workspaceRoot := protocol.DocumentURI(r.folder.URI).Path()
	return strings.HasPrefix(filename, workspaceRoot)
}

func (r *Resolver) PreloadWellKnownPaths() {
	for _, importName := range wellKnownModuleImports {
		r.findFileByPathLocked(importName, nil)
	}
}

func (r *Resolver) FindGeneratedFiles(uri protocol.DocumentURI, fd protoreflect.FileDescriptor) ([]ParsedGoFile, error) {
	return r.goLanguageDriver.FindGeneratedFiles(uri, fd.Options().(*descriptorpb.FileOptions), fd.Path())
}

func (r *Resolver) findImportPathsByPrefix(prefix string) map[protocol.DocumentURI]string {
	r.pathsMu.RLock()
	defer r.pathsMu.RUnlock()
	paths := map[protocol.DocumentURI]string{}
	for uri, path := range r.filePathsByURI {
		if strings.HasPrefix(path, prefix) {
			paths[uri] = path
		}
	}
	return paths
}

func FastLookupGoModule(f io.Reader) (string, error) {
	// Search the .proto file for `option go_package = "...";`
	// We know this will be somewhere at the top of the file.
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "option") {
			continue
		}
		index := strings.Index(line, "go_package")
		if index == -1 {
			continue
		}
		for ; index < len(line); index++ {
			if line[index] == '=' {
				break
			}
		}
		for ; index < len(line); index++ {
			if line[index] == '"' {
				break
			}
		}
		if index == len(line) {
			continue
		}
		startIdx := index + 1
		endIdx := strings.LastIndexByte(line, '"')
		endSemicolon := strings.IndexByte(line, ';')
		if endSemicolon > startIdx && endSemicolon < endIdx {
			endIdx = endSemicolon
		}
		if endIdx <= startIdx {
			continue
		}
		return line[startIdx:endIdx], nil
	}
	return "", fmt.Errorf("no go_package option found")
}

func (r *Resolver) tryReverseLookupLocked(path string, whence protocompile.ImportContext) (string, error) {
	if !r.goLanguageDriver.HasGoModule() {
		return "", ErrNoModule
	}

	fd := whence.FileDescriptorProto()
	uri, ok := r.fileURIsByPath[fd.GetName()]
	if !ok {
		return "", fmt.Errorf("source file %q has no URI", fd.GetName())
	}

	generated, err := r.goLanguageDriver.FindGeneratedFiles(uri, fd.Options, "")
	if err != nil {
		return "", err
	}

	if len(generated) == 0 {
		return "", fmt.Errorf("no generated files found for %q", fd.GetName())
	}

	resolved, err := tryResolvePathToGeneratedImport(generated, path, whence)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// Translates paths relative to either the importing file or the workspace root
func (r *Resolver) translatePathLocked(path string, whence protocompile.ImportContext) (string, error) {
	if _, ok := r.fileURIsByPath[path]; ok {
		// already a known path
		return path, nil
	}

	fd := whence.FileDescriptorProto()
	uri, ok := r.fileURIsByPath[fd.GetName()]
	if !ok {
		return "", fmt.Errorf("source file %q has no URI", fd.GetName())
	}

	var translatedPath string
	if !uri.IsFile() {
		return "", os.ErrNotExist
	}
	// simple cases:
	// 1. check if the path is relative to the source file
	filename := uri.Path()
	if filepath.IsLocal(path) { // does the path look like a local file (not absolute, no ../ etc)
		candidates := []string{
			filepath.Join(filepath.Dir(filename), path), // relative
		}
		if idx := strings.IndexRune(path, '/'); idx > 0 && path[:idx] == filepath.Base(filepath.Dir(filename)) {
			// relative, but the path prefix is duplicated
			candidates = append(candidates, filepath.Join(filepath.Dir(filepath.Dir(filename)), path))
		}
		// relative, but to a suffix-matched parent directory
		// for example, thanos:
		// store/
		// ├── hintspb/
		// │  └── hints.proto
		// ├── labelpb/
		// │  └── types.proto
		// └── storepb/
		//    ├── prompb/
		//    │  ├── remote.proto
		//    │  └── types.proto
		//    ├── rpc.proto
		//    └── types.proto
		//
		// importing "store/storepb/types.proto" from github.com/thanos-io/pkg/store/storepb/rpc.proto
		// will match the "store/storepb/" suffix and add "github.com/thanos-io/pkg/store/storepb/types.proto"
		// as a possible candidate.
		if match, ok := FindSuffixMatchedPath(path, filename); ok {
			candidates = append(candidates, match)
		}

		// relative but to the parent directory
		candidates = append(candidates, filepath.Join(filepath.Dir(filepath.Dir(filename)), path))

		for _, candidate := range candidates {
			if f, err := os.Stat(candidate); err == nil && f.Mode().IsRegular() {
				// found it, now translate back to a matching URI
				translatedPath = candidate
				break
			}
		}
	}

	if translatedPath == "" {
		return "", fmt.Errorf("could not find file %q relative to %q", path, uri)
	}
	translatedURI := protocol.URIFromPath(translatedPath)

	// translate back to a URI that matches the importing file
	switch r.importSourcesByURI[uri] {
	case SourceLocalGoModule:
		// fast path
		f, err := os.Open(translatedPath)
		if err != nil {
			return "", err // shouldn't happen
		}
		goPkg, err := r.LookupGoModule(translatedPath, f)
		f.Close()
		if err != nil {
			return "", err // could happen maybe
		}
		canonicalName := filepath.Join(goPkg, filepath.Base(translatedPath))
		r.filePathsByURI[translatedURI] = canonicalName
		r.fileURIsByPath[canonicalName] = translatedURI
		r.importSourcesByURI[translatedURI] = SourceLocalGoModule
		return canonicalName, nil
	case SourceGoModuleCache:
		originalDir := filepath.Dir(filename)
		// determine the relative movement from the original package to the new package
		// and apply it to the original package
		relative, err := filepath.Rel(originalDir, filepath.Dir(translatedPath))
		if err != nil {
			return "", err // perhaps
		}
		originalPkg := r.filePathsByURI[uri]
		canonicalName := filepath.Join(filepath.Dir(originalPkg), relative, filepath.Base(translatedPath))
		r.filePathsByURI[translatedURI] = canonicalName
		r.fileURIsByPath[canonicalName] = translatedURI
		r.importSourcesByURI[translatedURI] = SourceGoModuleCache
		return canonicalName, nil
	case SourceRelativePath:
		// it's already a relative path, so just make it relative to that one
		originalDir := filepath.Dir(filename)
		translatedPath, err := filepath.Rel(originalDir, translatedPath)
		if err == nil {
			return translatedPath, nil
		}
	default:
	}
	return "", os.ErrNotExist
}

type match struct {
	path  string
	score int
}

func FindSuffixMatchedPath(target, source string) (string, bool) {
	targetDir := filepath.Dir(target)
	if targetDir == "." {
		return filepath.Join(filepath.Dir(source), target), true
	}
	sourceDir := filepath.Dir(source)

	// github.com/thanos-io/thanos/pkg/store/storepb/rpc.proto
	//                                 store/storepb/prompb/types.proto
	//                                 store/storepb/types.proto

	targetParts := strings.Split(targetDir, "/") // ["store", "storepb", "prompb"] or ["store", "storepb"]
	sourceParts := strings.Split(sourceDir, "/") // ["github.com", "thanos-io", "thanos", "pkg", "store", "storepb"]
	if sourceParts[0] == "" {
		sourceParts[0] = "/" // make sure joining results in an absolute path
	}
	var matches []match
	for offset := 1; offset <= len(sourceParts); offset++ {
		// apply an offset as follows:
		// |"pkg"|"store"|"storepb"|
		// |     |       |         |"store"  |"labelpb"|"prompb"| <- offset 0 (for reference, not checked)
		// |     |       |"store"  |"labelpb"|"prompb" |          <- offset 1 (no match)
		// |     |"store"|"labelpb"|"prompb" |                    <- offset 2 (match, score=1)
		//        ^^^^^^^ (1)
		// |"pkg"|"store"|"storepb"|
		// |     |       |         |"store"  |"storepb"|"prompb"| <- offset 0 (for reference, not checked)
		// |     |       |"store"  |"storepb"|"prompb" |          <- offset 1 (no match)
		// |     |"store"|"storepb"|"prompb" |                    <- offset 2 (match, score=2)
		//        ^^^^^^^ ^^^^^^^^^ (2)
		// |"pkg"|"store"|"storepb"|
		// |     |       |         |"store"  |"storepb"| <- offset 0 (for reference, not checked)
		// |     |       |"store"  |"storepb"|           <- offset 1 (no match)
		// |     |"store"|"storepb"|                     <- offset 2 (match, score=2)
		//        ^^^^^^^ ^^^^^^^^^ (2)

		// increase the offset until the first part of the target matches the last part of the source
		// then score based on how many parts match

		sourceStart := len(sourceParts) - offset
		score := 0
		for i := 0; i < min(len(targetParts), len(sourceParts)-sourceStart); i++ {
			if targetParts[i] == sourceParts[sourceStart+i] {
				score++
			} else {
				break
			}
		}
		if score > 0 {
			matches = append(matches, match{
				path:  filepath.Join(append(append(sourceParts[:sourceStart], targetParts...), filepath.Base(target))...),
				score: score,
			})
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	if len(matches) == 1 {
		return matches[0].path, true
	}
	// sort by score
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].score > matches[j].score
	})
	return matches[0].path, true
}
