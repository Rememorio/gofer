// Package readbeforewrite prevents blind modifications of existing files.
// A successful read records the complete file revision in model history; one
// matching revision authorizes one later write to the same normalized path.
package readbeforewrite
