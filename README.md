# In Neovim



# Installing

1. Clone this repo
2. Build and install the protols binary: `go install ./cmd/protols`
3. Add this to your Neovim config
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

If you use Noice then you need to run
`:NoiceDisable` to get rid of the annoying errors
