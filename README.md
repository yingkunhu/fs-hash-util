# Purpose of the project

The project is used to maintain a tool which based on calculate hash of local files; and maintain the result into a file based database.

- The app should use golang to implement
- The generated binary should at least support MacOS - arm64; ubuntu - x64; ubuntu - arm64

# Features

## File-based DB

The result should keep in a file-based DB, easy for later query and maintenance

## Search and Hash

- Search and list files under specified folder, and record the folder content - including sub-folders and files into a db file;
- the db should has file-name, relative path name (starting from the root), create time stamp, last update time stamp, file size, hash using sha-256 (if file size 0, use a dummy value)
- should skip exception (like node_modules) by pattern ( regex based pattern)
- when re-scan, the file with same relative-path+file-name+file-size+last-update-time should be skipped, assuming not changed