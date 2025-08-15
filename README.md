# In Neovim

```lua
vim.api.nvim_create_autocmd("FileType", {
  pattern = "proto",
  callback = function()
    vim.lsp.start({
      name = "protols",
      cmd = { "/Users/jackrickards/src/github.com/monzo/protols/protols", "serve", "--stdio" },
      root_dir = vim.fs.dirname(vim.fs.find({ ".git" }, { upward = true })[1]) or vim.fn.getcwd(),
    })
  end,
})
```

# Installing

1. Clone this repo
2. Build and install the protols binary: `go install ./cmd/protols`
3. Install `vsce` if you don't have it: `npm install --global @vscode/vsce`
4. cd to editors/vscode, then run `vsce package`
5. Install the vsix plugin: `code --install-extension ./protols-vscode-<version>.vsix`

# Special Thanks

This project is derived from [bufbuild/protocompile](https://github.com/bufbuild/protocompile) and [jhump/protoreflect](https://github.com/jhump/protoreflect). Thanks to the buf developers for their fantastic work.

Several packages in <https://github.com/golang/tools> are used to build the language server. A minimal subset of its lsp-related packages are maintained as a library at <https://github.com/kralicky/tools-lite>.
