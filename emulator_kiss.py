#!/usr/bin/env python3
"""
KISS TNC Emulator for testing.
Acts as a KISS server (TCP) that echoes received frames.
"""

import argparse
import asyncio
import logging
import struct
import sys

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

KISS_FEND = 0xC0
KISS_FESC = 0xDB
KISS_TFEND = 0xDC
KISS_TFESC = 0xDD


def decode_kiss(data):
    """Decode a KISS frame."""
    if not data:
        return b''
    if data[0] == KISS_FEND and data[-1] == KISS_FEND:
        data = data[1:-1]
    
    result = bytearray()
    i = 0
    while i < len(data):
        b = data[i]
        if b == KISS_FESC:
            if i + 1 < len(data):
                next_b = data[i + 1]
                if next_b == KISS_TFEND:
                    result.append(KISS_FEND)
                elif next_b == KISS_TFESC:
                    result.append(KISS_FESC)
                i += 2
                continue
        result.append(b)
        i += 1
    return bytes(result)


def encode_kiss(data):
    """Encode data as a KISS frame."""
    result = bytearray([KISS_FEND, 0x00])
    for b in data:
        if b == KISS_FEND:
            result.extend([KISS_FESC, KISS_TFEND])
        elif b == KISS_FESC:
            result.extend([KISS_FESC, KISS_TFESC])
        else:
            result.append(b)
    result.append(KISS_FEND)
    return bytes(result)


class KISSEmulatorProtocol(asyncio.Protocol):
    def connection_made(self, transport):
        self.transport = transport
        logger.info(f"KISS emulator connected")
        self.buffer = b''
    
    def data_received(self, data):
        self.buffer += data
        while KISS_FEND in self.buffer:
            start = self.buffer.index(KISS_FEND)
            end = self.buffer.index(KISS_FEND, start + 1)
            frame = self.buffer[start:end + 1]
            self.buffer = self.buffer[end + 1:]
            
            raw = decode_kiss(frame)
            logger.info(f"Received KISS frame: {raw.hex()}")
            
            if raw:
                response = encode_kiss(raw)
                self.transport.write(response)
                logger.info(f"Sent response: {response.hex()}")
    
    def connection_lost(self, exc):
        logger.info(f"KISS emulator disconnected")


class KISSEmulator:
    def __init__(self, host='localhost', port=8001):
        self.host = host
        self.port = port
        self.server = None
    
    async def start(self):
        loop = asyncio.get_running_loop()
        self.server = await loop.create_server(
            KISSEmulatorProtocol,
            self.host, self.port
        )
        logger.info(f"KISS emulator listening on {self.host}:{self.port}")
        
        try:
            async with self.server:
                await self.server.serve_forever()
        except KeyboardInterrupt:
            logger.info("Shutting down...")


def main():
    parser = argparse.ArgumentParser(description='KISS TNC Emulator')
    parser.add_argument('--host', default='localhost')
    parser.add_argument('--port', type=int, default=8001)
    args = parser.parse_args()
    
    emulator = KISSEmulator(args.host, args.port)
    asyncio.run(emulator.start())


if __name__ == '__main__':
    main()
